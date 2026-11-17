package pool

import (
	"context"
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
	ReadBudget int
	// ReadTimeout is how long one reading may take.
	//
	// 卡住的账号不拖住整趟：读不到就当它不合格（规格 §5）。
	ReadTimeout time.Duration
}

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

	added := 0
	var lastErr error
	// 这一趟读了几次：预算是这一趟的，不是整天的。
	reads := 0
	// 读失败的号这一趟不再读：它刚才读不到，再读一次也是读不到，而
	// 预算是有限的（规格 §6.4）。
	failed := map[string]bool{}
	// 一趟最多试这么多候选：候选都读不出东西来的时候，别把整张表试
	// 完 —— 试到没有新的候选为止。
	for attempts := 0; attempts < want*3+3; attempts++ {
		if k.pool.Len() >= want {
			break
		}
		inPool := k.pool.IDs()
		candidates, err := k.source.Candidates(ctx, want+len(inPool), inPool)
		if err != nil {
			lastErr = err
			break
		}
		if len(candidates) == 0 {
			break
		}
		progressed := false
		for _, c := range candidates {
			if k.pool.Len() >= want {
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
				}
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
				continue
			}
			if snap.HasAmounts && snap.Remaining <= 0 {
				// 读到了：它没有额度。标记掉，别让它占着位置。
				k.markSpent(ctx, c.Account.ID, snap)
				continue
			}
			if k.pool.Admit(Entry{
				Account:   c.Account,
				Remaining: snap.Remaining,
				Known:     snap.HasAmounts,
			}) {
				added++
				progressed = true
			}
		}
		if !progressed {
			break
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
func (k *Keeper) markSpent(ctx context.Context, accountID string, snap store.UsageSnapshot) {
	until := time.Time{}
	if snap.ExpiresAt.After(time.Now()) {
		until = snap.ExpiresAt
	}
	if until.IsZero() {
		until = time.Now().Add(k.opts.Cooldown)
	}
	if k.marker == nil {
		return
	}
	_ = k.marker.SetExhausted(ctx, accountID, "", until, "quota")
}
