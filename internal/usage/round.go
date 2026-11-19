package usage

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/store"
)

// Reader reads one account's credits.
//
// 它是一个接口，manager.Manager 正好是这个接口：这一趟读额度的活不
// 需要知道 manager 的别的本事。
type Reader interface {
	// RefreshUsage reads an account's credits and windows and saves them.
	RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error)
}

// ReadingGuard is an optional part of a Reader: it keeps two rounds
// from reading the same account at the same time.
//
// 同一个账号同一时刻只读一次（规格 §3.3）：签到那一趟和热的那一趟可能
// 撞上，两边都去读，读的是同一次。
//
// A reader that has it is asked before every reading; a reader that has
// not is asked nothing, and every reading is read as it was before.
type ReadingGuard interface {
	// BeginReading says a round is about to read an account, and
	// whether it may. It says false when another round is reading that
	// account right now.
	BeginReading(accountID string) bool
	// EndReading says the reading of an account is done.
	EndReading(accountID string)
}

// Round reads the credits of the accounts that should be read.
//
// 一趟读额度有四道控：并发、每个账号的时限、一趟的预算、一趟最多读几
// 个。卡住的一个账号不能拖住整趟，一趟也不能读上一百个账号。
type Round struct {
	// Store is the database.
	Store *store.Store
	// Reader reads one account.
	Reader Reader
	// Hot gives the accounts that were used lately, the most used first.
	Hot func() []string
	// Cfg is the configuration.
	Cfg config.Config
}

// NewRound makes a round.
func NewRound(s *store.Store, r Reader, hot func() []string, cfg config.Config) *Round {
	return &Round{Store: s, Reader: r, Hot: hot, Cfg: cfg}
}

// The ranks the accounts are read in.
//
// 排在前面的先读：签到待补的卡上现在是错的，到期的马上要回轮转，从
// 没读过的排在热的前面（它是一次性的活儿，排在后面就永远排在后
// 面），热的刚变过，晚一轮没关系。
const (
	rankPending = iota
	rankDue
	rankNever
	rankHot
)

// retryWindow is how long a reading that failed is tried again.
//
// 读失败的账号要再读一遍（签到给的积分还在账本里等着），可是不能一
// 直重试：一个永远读不回来的账号（账本的批次没有期限、上游在挂）
// 会一直占着待补那一位，一趟的批次全被它拿走，从来没读过的账号就再
// 也读不到——而每个账号至少读一次是硬要求。
//
// 所以重试是有期限的：失败之后的一段时间里还再读（上游多半是暂时
// 的），过了这段时间就把这个账号放下，让别的账号先读。
//
// 它是一个常数，不是一个配置项：配置的键是规格定死了的。
const retryWindow = 10 * time.Minute

// checkinWindow is how far back a check-in can still matter.
//
// 签到给的积分七天有效（规格 §5.3），而「签到待补」问的是「这个号签到
// 给了积分，可是卡上还摆着旧数」—— 那种情况只有刚签到的号才有：积分过
// 期了，补读它读回来的也是过期数。
//
// 所以问「最近七天的签到」跟问「全部签到」是一回事，而后者在有历史的库
// 上慢得没法看（一年 36 万行：138 秒，而这一档每 30 秒问一次）。
const checkinWindow = 7 * 24 * time.Hour

// target is one account to read, and where it stands in the order.
type target struct {
	// id is the account.
	id string
	// rank is the group it belongs to.
	rank int
	// order is what breaks a tie inside the rank.
	order int
	// since is when it began to wait, for the ranks that wait.
	since time.Time
}

// Targets gives the accounts to read now, in the order they should be
// read.
//
// 该读的就是那四类：签到待补的、标记到期的、从没读过的、热的。冷账号
// 不在名单里：它的数是旧的，读它只是把旧的数再读一遍——它被用到的时候
// 自己会醒过来（旧的数是高估，轮转会挑它），那时候它就热了。
func (r *Round) Targets(ctx context.Context) ([]string, error) {
	accounts, err := r.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	snaps, err := r.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	checkins, err := r.Store.CheckinTimes(ctx, now.Add(-checkinWindow))
	if err != nil {
		return nil, err
	}
	exhausted, err := r.Store.ListExhausted(ctx)
	if err != nil {
		return nil, err
	}

	snapAt := map[string]time.Time{}
	snapHas := map[string]bool{}
	for _, s := range snaps {
		snapAt[s.AccountID] = s.CreatedAt
		snapHas[s.AccountID] = s.HasAmounts
	}
	checkAt := map[string]time.Time{}
	for _, c := range checkins {
		checkAt[c.AccountID] = c.At
	}
	// 标记到期：标记过了时刻，账号该回轮转里读一次了。
	//
	// 到期读一次就好：读过了（数变新了）它就不再到期。还有一个标记没
	// 过时刻的，那个组还欠着额度，账号回不了轮转。
	expiredAt := map[string]time.Time{}
	stillSpent := map[string]bool{}
	for _, e := range exhausted {
		if e.Until.After(now) {
			stillSpent[e.AccountID] = true
			continue
		}
		if at, ok := expiredAt[e.AccountID]; !ok || e.Until.After(at) {
			expiredAt[e.AccountID] = e.Until
		}
	}
	hotAt := map[string]int{}
	if r.Hot != nil {
		for i, id := range r.Hot() {
			hotAt[id] = i
		}
	}

	ts := make([]target, 0, len(accounts))
	for _, a := range accounts {
		if a.Status != store.StatusActive {
			// 签出的账号读不到：读了也是白读。
			continue
		}
		read, hasSnap := snapAt[a.ID]
		checked := checkAt[a.ID]
		// 签到待补：签到过了，账本还没读到。
		//
		// 签到是把它放进这个名单的那一件事：签到给了积分，可账本还没
		// 读到，卡上就还是旧的。
		//
		// 读到的那次失败了也要再读，可是只再读一会儿（retryWindow）：
		// 不限次数地重试，一个一直读不回来的账号就把整趟的批次占住
		// 了，从来没读过的账号再也读不到。
		pending := false
		if !checked.IsZero() {
			pending = !hasSnap || checked.After(read)
			if !pending && !snapHas[a.ID] && now.Sub(checked) < retryWindow {
				pending = true
			}
		}
		// 标记到期：标记过了时刻，而且没人读过它。
		//
		// 读过了就不到期了：到期读一次，数变新，账号回轮转。
		due := false
		if until, ok := expiredAt[a.ID]; ok && !stillSpent[a.ID] {
			due = !hasSnap || read.Before(until)
		}
		switch {
		case pending:
			ts = append(ts, target{id: a.ID, rank: rankPending, since: checked})
		case due:
			ts = append(ts, target{id: a.ID, rank: rankDue, since: expiredAt[a.ID]})
		case !hasSnap:
			// 从没读过的：卡上没有它，而且它永远排在最后就永远读不到。
			ts = append(ts, target{id: a.ID, rank: rankNever})
		default:
			if at, ok := hotAt[a.ID]; ok {
				ts = append(ts, target{id: a.ID, rank: rankHot, order: at})
			}
			// 冷的：不读。它的数是旧的，被用到的时候它自己会醒过来。
		}
	}

	// 排在前面的先读。同一组里：等得最久的先（签到待补的按签到的时
	// 刻，标记到期的按到期的时刻），热的按用得多的先，其余按 id 分出
	// 来——结果每次一样。
	sort.Slice(ts, func(i, j int) bool {
		if ts[i].rank != ts[j].rank {
			return ts[i].rank < ts[j].rank
		}
		switch ts[i].rank {
		case rankPending, rankDue:
			if !ts[i].since.Equal(ts[j].since) {
				return ts[i].since.Before(ts[j].since)
			}
		case rankHot:
			if ts[i].order != ts[j].order {
				return ts[i].order < ts[j].order
			}
		}
		return ts[i].id < ts[j].id
	})

	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.id)
	}
	return out, nil
}

// Options holds the controls of one round of readings.
type Options struct {
	// Concurrency is how many accounts are read at once.
	Concurrency int
	// Timeout is how long one account's reading may take.
	Timeout time.Duration
	// Budget is how long the whole round may take.
	Budget time.Duration
	// Batch is how many accounts the round reads at most.
	Batch int
}

// Result says what one round of readings came to.
type Result struct {
	// Read are the readings that came back.
	Read []store.UsageSnapshot
	// Refreshed counts the accounts whose credits were read.
	Refreshed int
	// Failed counts the accounts whose credits could not be read.
	Failed int
	// Skipped counts the accounts the round did not get to.
	Skipped int
	// Reasons says why, one line per account.
	Reasons []string
}

// Run reads the given accounts' credits, within a budget.
//
// 同一个账号同一时刻只读一次：一个账号在名单里出现两遍，读它一遍就
// 够了。读失败不挡别的账号。
func (r *Round) Run(ctx context.Context, ids []string, opts Options) (Result, error) {
	var out Result
	ids = dedupe(ids)
	if len(ids) == 0 {
		return out, nil
	}
	opts.Batch = batch(opts.Batch)
	if len(ids) > opts.Batch {
		out.Skipped = len(ids) - opts.Batch
		ids = ids[:opts.Batch]
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	if opts.Budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Budget)
		defer cancel()
	}
	// guard 是读者跟别的那一趟之间的那把锁：没有它，就跟从前一样。
	guard, hasGuard := r.Reader.(ReadingGuard)
	sem := make(chan struct{}, opts.Concurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			skipped := func() {
				mu.Lock()
				out.Skipped++
				out.Reasons = append(out.Reasons, id+": the budget is used up")
				mu.Unlock()
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				skipped()
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				// 抢到了位置，可是预算刚好在这时候用完了：这个账号没读
				// 成，算没轮上，不算读失败。
				//
				// 不查这一下就说不准：select 的两个都成了的时候，Go 随
				// 便挑一个，于是同一个账号有时算跳过，有时算失败。
				skipped()
				return
			}
			if hasGuard && !guard.BeginReading(id) {
				// 别的这一趟正在读这个账号：这一趟不读它。读的是同
				// 一次，读两遍是白读（规格 §3.3）。
				//
				// 这不是失败，也不是没轮上：它很正常，所以不算，也不
				// 记一笔理由。
				return
			}
			if hasGuard {
				// 读完了（读失败也是读完了）就把账号还回去。
				defer guard.EndReading(id)
			}
			one := ctx
			if opts.Timeout > 0 {
				var cancel context.CancelFunc
				one, cancel = context.WithTimeout(ctx, opts.Timeout)
				defer cancel()
			}
			snap, err := r.Reader.RefreshUsage(one, id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out.Failed++
				out.Reasons = append(out.Reasons, id+": "+err.Error())
				return
			}
			out.Refreshed++
			out.Read = append(out.Read, snap)
		}(id)
	}
	wg.Wait()
	return out, nil
}

// RefreshDue reads the credits of the accounts that should be read.
func (r *Round) RefreshDue(ctx context.Context) (Result, error) {
	ids, err := r.Targets(ctx)
	if err != nil {
		return Result{}, err
	}
	return r.Run(ctx, ids, r.Options())
}

// RefreshHot reads the credits of the accounts that were used lately.
//
// 卡片那条路：只现读最热的几个，其余用上一次读到的数。卡片因此永远
// 几秒内回来。
func (r *Round) RefreshHot(ctx context.Context) (Result, error) {
	var ids []string
	if r.Hot != nil {
		ids = r.Hot()
	}
	return r.Run(ctx, ids, r.CardOptions())
}

// Options gives the controls of the periodic round.
func (r *Round) Options() Options {
	f := r.Cfg.Usage.Refresh
	return Options{
		Concurrency: f.Concurrency,
		Timeout:     f.Timeout.Duration(),
		Budget:      f.Budget.Duration(),
		Batch:       batch(f.Batch),
	}
}

// CardOptions gives the controls of the card's own round.
//
// 卡片有自己的一份预算：它不能等。它读的那些账号的时限也是这份预算
// 说了算，不然一个卡住的账号会把卡片拖到超时。
func (r *Round) CardOptions() Options {
	f := r.Cfg.Usage.Refresh
	timeout := f.Timeout.Duration()
	if b := f.CardBudget.Duration(); b > 0 && (timeout <= 0 || b < timeout) {
		timeout = b
	}
	return Options{
		Concurrency: f.Concurrency,
		Timeout:     timeout,
		Budget:      f.CardBudget.Duration(),
		Batch:       batch(f.Card),
	}
}

// batch gives how many accounts one round reads at most.
//
// 配 0 的时候按 1 算：0 个一批是一趟也读不了，那不是关掉，是坏了。
// 卡片那一趟也一样：card: 0 该是"读一个"，不是"全都读"。
func batch(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}

// HasRoom says whether a reading says the account has something to spend.
//
// 账本还有余额，或者计划窗口还有余量，都算还有。读不到（没有账本，
// 窗口也没有余量）就是没有：宁可少用一个账号，也不要把一个用完的
// 账号放回去让客户端撞它。
func HasRoom(snap store.UsageSnapshot) bool {
	if snap.HasAmounts && snap.Remaining > 0 {
		return true
	}
	for _, w := range windowsOf(snap) {
		if w.Unlimited {
			return true
		}
		if w.Limit > 0 && w.Amount < w.Limit {
			return true
		}
	}
	return false
}

// windowsOf reads the plan's windows out of a reading.
func windowsOf(snap store.UsageSnapshot) []account.Window {
	if snap.WindowsJSON == "" {
		return nil
	}
	var ws []account.Window
	if err := json.Unmarshal([]byte(snap.WindowsJSON), &ws); err != nil {
		return nil
	}
	return ws
}

// ClearSpent takes the "used up" marks off an account.
//
// 它是给签到用的：签到给的积分能救活一个用完的账号，而清不清由读到
// 的数决定。
func (r *Round) ClearSpent(ctx context.Context, accountID string) error {
	rows, err := r.Store.ListExhausted(ctx)
	if err != nil {
		return err
	}
	for _, e := range rows {
		if e.AccountID != accountID {
			continue
		}
		if err := r.Store.ClearExhausted(ctx, e.AccountID, e.WindowKey); err != nil {
			return err
		}
	}
	return nil
}

// Settle reads an account after a check-in and says whether it is spent.
//
// 它是签到那条路的一整套：读完，读到的数说有额度就清掉"用完"的标
// 记，读不到就留着（账号进"待补"名单，下一趟再读）。
func (r *Round) Settle(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	snap, err := r.Reader.RefreshUsage(ctx, accountID)
	if err != nil {
		return snap, err
	}
	if !HasRoom(snap) {
		return snap, nil
	}
	if err := r.ClearSpent(ctx, accountID); err != nil {
		return snap, err
	}
	return snap, nil
}

// dedupe takes the repeated ids out of a list, keeping the order.
func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
