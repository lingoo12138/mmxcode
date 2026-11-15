package usage

import (
	"context"
	"sort"
	"sync"
	"time"

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
// 面），热的刚变过，晚一轮没关系，其余的最久没读的先。
const (
	rankPending = iota
	rankDue
	rankNever
	rankHot
	rankCold
)

// target is one account to read, and where it stands in the order.
type target struct {
	// id is the account.
	id string
	// rank is the group it belongs to.
	rank int
	// order is what breaks a tie inside the rank.
	order int
}

// Targets gives the accounts to read now, in the order they should be
// read.
//
// 该读的排在前面：签到待补的、标记到期的、从没读过的、热的。冷账号也
// 在名单里，排在最后：一趟最多读几个（批次），排在前面的是该读的，
// 冷账号排在后面——它被读到的时候读到的数也是旧的。
func (r *Round) Targets(ctx context.Context) ([]string, error) {
	accounts, err := r.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	snaps, err := r.Store.LatestUsageSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	checkins, err := r.Store.LatestCheckins(ctx)
	if err != nil {
		return nil, err
	}
	exhausted, err := r.Store.ListExhausted(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()

	snapAt := map[string]time.Time{}
	snapHas := map[string]bool{}
	for _, s := range snaps {
		snapAt[s.AccountID] = s.CreatedAt
		snapHas[s.AccountID] = s.HasAmounts
	}
	checkAt := map[string]time.Time{}
	for _, c := range checkins {
		checkAt[c.AccountID] = c.CreatedAt
	}
	// 标记到期：标记的时刻到了，账号回到轮转里，但没有人读过它。
	due := map[string]bool{}
	for _, e := range exhausted {
		if e.Until.After(now) {
			continue
		}
		due[e.AccountID] = true
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
		// 读到（没读到，或者读到的那次失败了），卡上就还是旧的。
		pending := !checked.IsZero() &&
			(!hasSnap || checked.After(read) || !snapHas[a.ID])
		switch {
		case pending:
			ts = append(ts, target{id: a.ID, rank: rankPending})
		case due[a.ID]:
			ts = append(ts, target{id: a.ID, rank: rankDue})
		case !hasSnap:
			// 从没读过的：卡上没有它，而且它永远排在最后就永远读不到。
			ts = append(ts, target{id: a.ID, rank: rankNever})
		default:
			if at, ok := hotAt[a.ID]; ok {
				ts = append(ts, target{id: a.ID, rank: rankHot, order: at})
				continue
			}
			ts = append(ts, target{id: a.ID, rank: rankCold})
		}
	}

	// 排在前面的先读。同一组里：冷账号最久没读的先（越旧越可能过期），
	// 热的按用得多的先，其余按 id 分出来——结果每次一样。
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].rank != ts[j].rank {
			return ts[i].rank < ts[j].rank
		}
		if ts[i].rank == rankCold {
			if !snapAt[ts[i].id].Equal(snapAt[ts[j].id]) {
				return snapAt[ts[i].id].Before(snapAt[ts[j].id])
			}
		} else if ts[i].order != ts[j].order {
			return ts[i].order < ts[j].order
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
	if opts.Batch > 0 && len(ids) > opts.Batch {
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
		Batch:       f.Batch,
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
		Batch:       f.Card,
	}
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
