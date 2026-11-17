package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"mmxcode/internal/store"
)

// Source gives the accounts that could be admitted to the pool.
type Source interface {
	// Candidates gives the accounts that are not in the pool, coldest
	// first. The exclude map holds the ids that are in it already; a
	// source that does not need it may ignore it.
	Candidates(ctx context.Context, limit int, exclude map[string]bool) ([]store.Candidate, error)
}

// Reader reads an account's credits.
type Reader interface {
	// RefreshUsage reads an account's credits and saves them.
	RefreshUsage(ctx context.Context, accountID string) (store.UsageSnapshot, error)
}

// Marker marks an account's window spent.
type Marker interface {
	// SetExhausted marks an account's window spent. The order is the
	// account, the window key, the time it comes back and the reason.
	SetExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error
}

// KeeperOptions is how the keeper behaves.
type KeeperOptions struct {
	// Read says a newly admitted account's credits are read before it
	// goes in.
	Read bool
	// Cooldown is how long an account that reads as spent is given up
	// on when the reading says no moment.
	Cooldown time.Duration
	// ReadBudget is how many accounts one pass may read the credits of.
	//
	// 0 = 不限 —— 别这么配：补号读的是上游的钱，一次读是 2~11 个上游请求
	//（规格 §6.4），一趟读几百个号就是几千个请求，那不是填池，是打上游。
	// 预算就是 usage.refresh.batch：那一趟该读几个，就读几个。
	//
	// 生产上不要拿 0 来表示"不限"：调用它的那一层（gateway）必须先把它
	// 夹住 —— 那边 0 的意思是"一个一批"，不是"全都读"。
	ReadBudget int
	// ReadTimeout is how long one reading may take.
	//
	// 卡住的账号不拖住整趟：读不到就当它不合格（规格 §5）。
	ReadTimeout time.Duration
	// PassTimeout is how long one pass may take.
	//
	// 一趟补号的上限：候选是问出来的，读额度是问上游的，两样都要时间。
	// 没有这个上限，一趟的时长是"预算 × 每个号的时限"（6 × 15 秒 = 90
	// 秒），而补号那一趟每 30 秒就该来一次 —— 中间那几趟都被"上一趟还
	// 在跑"挡掉了，补号的速度就悄悄降下来了。
	//
	// 0 = 不限。生产上不该这么配。
	PassTimeout time.Duration
}

// markWriteTimeout is how long the writing of a mark may take.
//
// 写一行标记是本地库里的一个写，几毫秒的事：5 秒钟是给卡住的库留的余
// 量，不是给它等的。
const markWriteTimeout = 5 * time.Second

// Keeper keeps the pool full.
//
// 两个动作：
//
//   - Seed：启动时填池。**不问站点** —— 用库里最后读到的数顶上，等不
//     起（读额度是 2~11 个上游请求，填满 400 个号要半个多小时）。
//   - Replenish：池里少了号，补进来。补进来的号**读一次额度**：读不
//     到就别进池，读到「用完了」就把它标记掉，换下一个。
type Keeper struct {
	pool   *Pool
	source Source
	reader Reader
	marker Marker
	// opts is how the keeper behaves.
	opts KeeperOptions
	// mu guards running.
	mu sync.Mutex
	// running says a replenishment is in the air.
	//
	// 一次只跑一趟：补号是上游的活，跑两趟就是两倍的请求，而池里的
	// 号不会因为跑两趟就多出来。
	running bool
}

// NewKeeper makes a keeper.
func NewKeeper(p *Pool, source Source, reader Reader, marker Marker, opts KeeperOptions) *Keeper {
	if opts.Cooldown <= 0 {
		opts.Cooldown = 5 * time.Minute
	}
	return &Keeper{
		pool:   p,
		source: source,
		reader: reader,
		marker: marker,
		opts:   opts,
	}
}

// Pool gives the pool the keeper fills.
func (k *Keeper) Pool() *Pool { return k.pool }

// Seed fills the pool with what the database knows, without asking the
// site for anything.
//
// It says how many accounts went in.
//
// 冷启动用这个：池要立刻能用，而读额度等不起（一次读是 2~11 个上游请
// 求）。旧数只是高估 —— 撞空时换号重试会把标记写准，自愈。
func (k *Keeper) Seed(ctx context.Context, want int) (int, error) {
	if want <= 0 {
		return 0, nil
	}
	candidates, err := k.source.Candidates(ctx, want, k.pool.IDs())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range candidates {
		if k.pool.Len() >= want {
			break
		}
		if !k.pool.Admit(Entry{
			Account:   c.Account,
			Remaining: c.Remaining,
			Known:     c.Known,
		}) {
			continue
		}
		n++
	}
	return n, nil
}

// Replenish puts accounts into the pool until it holds want of them.
//
// It says how many accounts went in.
//
// 补进来的号读一次额度：读到的数说有额度，它进池；读到「用完了」，它
// 不进池，而且被标记掉（它留在库里占着一个位置，下次补号不会再挑它）。
//
// 一次只跑一趟：已经在跑的那一趟没有跑完，这一次就不跑 —— 池里的号不
// 会因为跑两趟就多出来，而上游的请求会翻倍。
//
// 出错的时候说的是这一趟出的错：候选问不到、标记写不进去、这一趟被它
// 自己的时限掐断。三件都说出去 —— 池短着的时候，日志里要看得见是"没
// 填上"还是"没跑完"。
func (k *Keeper) Replenish(ctx context.Context, want int) (int, error) {
	if want <= 0 {
		return 0, nil
	}
	k.mu.Lock()
	if k.running {
		k.mu.Unlock()
		return 0, nil
	}
	k.running = true
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		k.running = false
		k.mu.Unlock()
	}()

	// 这一趟自己的时限：候选是问出来的，读额度是问上游的，两样都要时间。
	// 没有它，一趟能跑"预算 × 每个号的时限"，而补号那一趟每 30 秒就该来
	// 一次 —— 中间那几趟都被"上一趟还在跑"挡掉了，补号的速度悄悄降下来。
	if k.opts.PassTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, k.opts.PassTimeout)
		defer cancel()
	}

	added := 0
	var lastErr error
	// markErr is the mark that could not be written.
	//
	// 它跟 lastErr 分开记：两个错都要给调用方 —— 少一个，日志里就少一件
	// 事：一个是这一趟没填上，一个是那个号还在库里占着位置。
	var markErr error
	// 这一趟读了几次：预算是这一趟的，不是整天的。
	reads := 0
	// 读失败的号这一趟不再读：它刚才读不到，再读一次也是读不到，而
	// 预算是有限的（规格 §6.4）。
	failed := map[string]bool{}
	// 这一趟挑过了几个：候选里被刷掉的那些。下一趟问候选的时候要多问
	// 这么多个 —— 不然问回来的还是那几个刷掉的。
	passed := 0
	// cutShort says the pass was cut short by its own deadline.
	//
	// 它跟"这一趟没填上"是两件事：填池没填满是结果，被时限掐断是原因。
	// 掐断了还报"没有错"，池短着的时候日志里就什么都看不见 —— 只看得见
	// 池短了，看不见为什么。
	cutShort := false
	// 一趟最多试这么多候选：候选都读不出东西来的时候，别把整张表试
	// 完 —— 试到没有新的候选为止。
	for attempts := 0; attempts < want*3+3; attempts++ {
		if k.pool.Len() >= want {
			break
		}
		if ctx.Err() != nil {
			// 这一趟的时间用完了：整趟就此打住，不是只放下一个号。下
			// 一趟重新开始。
			cutShort = true
			break
		}
		inPool := k.pool.IDs()
		candidates, err := k.source.Candidates(ctx, want+len(inPool)+passed, inPool)
		if err != nil {
			lastErr = err
			break
		}
		if len(candidates) == 0 {
			break
		}
		progressed := false
		// 这一轮里新刷掉的候选：全是老面孔就是没有新消息，别再问了。
		fresh := 0
		for _, c := range candidates {
			if k.pool.Len() >= want {
				break
			}
			if ctx.Err() != nil {
				// 这一趟的时间用完了：不再读下一个号，也不再问候选。
				cutShort = true
				break
			}
			if inPool[c.Account.ID] {
				continue
			}
			if failed[c.Account.ID] {
				continue
			}
			if !k.opts.Read {
				if k.pool.Admit(Entry{
					Account:   c.Account,
					Remaining: c.Remaining,
					Known:     c.Known,
				}) {
					added++
					progressed = true
					continue
				}
				passed++
				fresh++
				continue
			}
			if k.opts.ReadBudget > 0 && reads >= k.opts.ReadBudget {
				// 这一趟的预算用完了：剩下的号这一趟不进池 —— 读不到就
				// 不进（规格 §5），下一趟再说。池子短一点，总并发就
				// 少承诺一点（不变量 4），那比打上游打疼了强。
				break
			}
			reads++
			snap, err := k.readOne(ctx, c.Account.ID)
			if err != nil {
				// 读不到：不知道有没有额度，就别进池（进池了挑号也挑不
				// 到它 —— 没读到数的号排在最后）。这一趟就当它不合格。
				failed[c.Account.ID] = true
				passed++
				fresh++
				continue
			}
			if snap.HasAmounts && snap.Remaining <= 0 {
				// 读到了：它没有额度。标记掉，别让它占着位置。这一趟
				// 也不再读它 —— 读到了就是没有，再读一遍还是没有，而
				// 预算是有限的。
				if err := k.markSpent(ctx, c.Account.ID, snap); err != nil {
					// 写不进去：那个号在库里还占着一个位置，下一趟
					// 补号还会挑它，再读一遍（2~11 个上游请求），一趟
					// 接一趟。记下来，别让它悄悄地没有。
					//
					// 这一趟不因为它放弃：一个号的标记写不进去，不该
					// 把整趟填池都扔掉。
					if markErr == nil {
						markErr = fmt.Errorf("the mark of %s could not be written: %w", c.Account.ID, err)
					}
				}
				failed[c.Account.ID] = true
				passed++
				fresh++
				continue
			}
			if k.pool.Admit(Entry{
				Account:   c.Account,
				Remaining: snap.Remaining,
				Known:     snap.HasAmounts,
			}) {
				added++
				progressed = true
				continue
			}
			passed++
			fresh++
		}
		if !progressed && fresh == 0 {
			// 这一轮什么也没进池，问回来的还是那几个刷掉的：没有新消
			// 息，别再问了。下一趟重新问。
			break
		}
	}
	// 这一趟出的错都要说出去：候选问不到是一件事，标记写不进去是另一件
	// —— 少一件，日志里就少一件。只有一件的时候就是那一个：调用方拿
	// 到的就是那个错本身。
	switch {
	case lastErr != nil && markErr != nil:
		lastErr = errors.Join(lastErr, markErr)
	case lastErr == nil:
		lastErr = markErr
	}
	if cutShort {
		// 这一趟是被时限掐断的：也说出去 —— 池短着的时候，日志里要看得
		// 见是"这一趟没跑完"，不是"这一趟跑完了，池还是短"。
		//
		// 别的事也有错的时候，两件都留着：那个错是这一趟的错，这个错是
		// 这一趟为什么没跑完。
		if lastErr == nil {
			lastErr = ctx.Err()
		} else {
			lastErr = errors.Join(lastErr, ctx.Err())
		}
	}
	return added, lastErr
}

// readOne reads one account's credits, with the timeout the keeper has.
//
// 卡住的账号不拖住整趟：读它花掉的时间有一个上限（规格 §5）。
func (k *Keeper) readOne(ctx context.Context, accountID string) (store.UsageSnapshot, error) {
	if k.opts.ReadTimeout <= 0 {
		return k.reader.RefreshUsage(ctx, accountID)
	}
	rctx, cancel := context.WithTimeout(ctx, k.opts.ReadTimeout)
	defer cancel()
	return k.reader.RefreshUsage(rctx, accountID)
}

// markSpent writes the mark of an account that reads as spent.
//
// 窗口键是空的：那是账号级的标记，对所有窗口组都算 —— 读到的积分是零，
// 说的不是「这个模型组用完了」，是「这个号没有额度了」。
//
// 什么时候回来：读到的时间在将来就用它，取不到就用冷却时间。
//
// 写这一行用的是不跟这一趟的时限走的 ctx：这一趟的时限到了，不该让这
// 个标记丢掉。丢了这个号还在库里占着一个位置，下一趟补号还会挑它，再
// 读一遍（一次读是 2~11 个上游请求），一趟接一趟，直到它自己好起来。
// 标记是这个号的事实，不是一趟的活。
func (k *Keeper) markSpent(ctx context.Context, accountID string, snap store.UsageSnapshot) error {
	until := time.Time{}
	if snap.ExpiresAt.After(time.Now()) {
		until = snap.ExpiresAt
	}
	if until.IsZero() {
		until = time.Now().Add(k.opts.Cooldown)
	}
	if k.marker == nil {
		return nil
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markWriteTimeout)
	defer cancel()
	return k.marker.SetExhausted(wctx, accountID, "", until, "quota")
}
