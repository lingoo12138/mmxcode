package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/lingoo12138/mmxcode/internal/config"
)

// busyAccounts counts the requests in the air per account.
//
// 它是内存里的账：网关自己知道有几个请求在飞，不用问数据库。
type busyAccounts struct {
	mu sync.Mutex
	// per is how many requests one account takes at once.
	per int
	// at counts the requests in the air per account.
	at map[string]int
}

// newBusyAccounts makes the counter.
func newBusyAccounts(per int) *busyAccounts {
	if per <= 0 {
		per = 1
	}
	return &busyAccounts{per: per, at: map[string]int{}}
}

// take says a request takes an account, and whether it could.
//
// 抢不到就是抢不到：这个账号正在飞，第二个请求要等它做完，或者换一个
// 账号。
func (b *busyAccounts) take(accountID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at[accountID] >= b.per {
		return false
	}
	b.at[accountID]++
	return true
}

// give says a request is done with an account.
func (b *busyAccounts) give(accountID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at[accountID] <= 1 {
		delete(b.at, accountID)
		return
	}
	b.at[accountID]--
}

// isBusy says whether a request is in the air on an account.
//
// 它是 rotator 问的那一句：这个账号现在忙不忙（规格 §6.3）。
func (b *busyAccounts) isBusy(accountID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.at[accountID] >= b.per
}

// count says how many accounts have a request in the air.
func (b *busyAccounts) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.at)
}

// waitForFree waits until an account can answer, or the deadline is over.
//
// 等的是一个能答的账号，不是整个网关空下来：别的账号在飞的时候，这个
// 请求照样能走到那个空着的账号上（规格 §6.4）。
//
// 账本只记着在飞的那几个账号，它不知道池子里有多少账号：所以"还有没
// 有能答的账号"只能问 rotator —— 谁在飞、谁能用、谁被标记用完了，只有
// 它知道。
//
// 等不到就是等不到：客户端自己超时了，网关不该把它吊着。
func (s *Server) waitForFree(ctx context.Context, deadline time.Time, model string, used map[string]bool) bool {
	for {
		// 每一次都先睡一下再看：刚空出来的那个账号可能又被别的请求
		// 抢走了，一醒就轮着看会转不停。
		if !sleepCtx(ctx, 25*time.Millisecond) {
			return false
		}
		// 先问一句便宜的：在飞的和试过的加起来，比池里能答的号少，那就
		// 一定有空位 —— 这一句是够的，它是个充足条件。其余的都去问
		// rotator：谁在飞、谁能用、谁被标记用完了，只有它知道，而那一
		// 次要把池里的名单拷一份（池 400 个号就是 400 行 × 每次等待，
		// 一秒 40 次）。
		if s.Pool != nil && s.busy != nil && s.Pool.Len() > 0 {
			free := s.Pool.Usable() - s.busy.count() - len(used)
			if free > 0 {
				// 一定有空位：池里能答的号比在飞的加试过的还多，那一定
				// 有一个既不忙也没试过。
				return true
			}
			// 其余的都说不准：free 是个下限，不是确数 —— 在飞的那几个
			// 和试过的那几个里面，有的不在池里（池空的时候从库里拿的
			// 号，还有用完了出了池、请求还在飞的号）。所以负数说的是
			// "不知道"，不是"没有"：当成"没有"就跳过了 rotator，而
			// rotator 才是那个知道有没有空位的。
		}
		ok, err := s.Rot.AnyUsable(ctx, model, used)
		if err == nil && ok {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
	}
}

// gate counts the requests in the air and lets the ones that do not
// fit wait.
type gate struct {
	mu sync.Mutex
	// inFlight is how many requests are in the air.
	inFlight int
}

// take lets a request in when there is room under limit.
func (g *gate) take(limit int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if limit < 1 {
		limit = 1
	}
	if g.inFlight >= limit {
		return false
	}
	g.inFlight++
	return true
}

// give says a request is done.
func (g *gate) give() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight > 0 {
		g.inFlight--
	}
}

// waitFor waits until there is room under limit, or the wait is over.
//
// 等不到就是等不到：客户端自己超时了，网关不该把它吊着。
func (g *gate) waitFor(ctx context.Context, limit int, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if g.take(limit) {
			return true
		}
		if !sleepCtx(ctx, 25*time.Millisecond) {
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// totalFor gives the total concurrency for a number of usable accounts.
//
// 总并发 = max(可用账号数 ÷ total_ratio, min(可用账号数, total_min))，
// 再按天花板截下来
//
// 五分之一在用，四分之五留着：100 个账号同时只用 20 个，这 20 个把额
// 度用完了，还有 80 个是满的可以接。小规模的时候至少 min(账号数, 3)
// 个：5 个账号只跑 1 个太憋屈。（比例和底都是配置说了算：
// concurrency.total_ratio 和 concurrency.total_min，默认 5 和 3。）
func totalFor(usable int, cfg config.Concurrency) int {
	ratio := cfg.TotalRatio
	if ratio < 1 {
		ratio = 1
	}
	floor := cfg.TotalMin
	if floor < 1 {
		floor = 1
	}
	if usable < floor {
		floor = usable
	}
	total := usable / ratio
	if total < floor {
		total = floor
	}
	if total < 1 {
		total = 1
	}
	// 天花板：运营定的那根杆（规格 §6.9）。它在这里，不在
	// totalConcurrency 里 —— 池的大小是按承诺的并发算的
	// （max(总并发, 活跃会话数) × 倍数），天花板压承诺的时候，池也跟着小。
	if top := cfg.TotalMax; top > 0 && total > top {
		total = top
	}
	return total
}

// totalConcurrency says how many requests may be in the air at once.
//
// 承诺的并发 = min(期望并发, 池里能用的号数)：
//
//   - 期望并发 = 可用账号数 ÷ 5（小规模至少 3），按天花板截下来，跟从前一样；
//   - 池里能用的号数 = 池里签入中、没有被标记用完的号。
//
// 池里没有那么多号，就只能承诺那么多 —— 承诺了接不住就是一片 429（规格
// 不变量 4）。
//
// 池是空的（还没填上，或者填池失败）的时候从库里数：网关得能干活。但
// 那一下能从 20 抬到 80（库里 400 个号 ÷ 5），所以天花板正好是给这一
// 下准备的：它把承诺按在运营定的那个数上。
func (s *Server) totalConcurrency(ctx context.Context) int {
	if s.Store == nil {
		return 1
	}
	if s.Pool == nil {
		// 没有池就没有「池夹」这回事：承诺还是从前那样按库里数。
		return 1
	}
	usable, err := s.Store.CountUsable(ctx)
	if err != nil {
		return 1
	}
	want := totalFor(usable, s.Cfg.Concurrency)
	inPool := s.Pool.Usable()
	switch {
	case inPool == 0:
		// 池是空的：还没填上，或者填池失败。跟从前一样，从库里数。
	case inPool < want:
		// 池里的号不够：只能承诺这么多。
		want = inPool
	}
	return want
}
