package gateway

import (
	"context"
	"sync"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/store"
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
// 总并发 = max(floor(可用账号数 / 5), min(可用账号数, 3))
//
// 五分之一在用，四分之五留着：100 个账号同时只用 20 个，这 20 个把额
// 度用完了，还有 80 个是满的可以接。小规模的时候至少 min(账号数, 3)
// 个：5 个账号只跑 1 个太憋屈。
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
	return total
}

// totalConcurrency says how many requests may be in the air at once.
//
// 可用账号数 = 状态是 active（签入中）且没有被标记用完的账号数。账号
// 用完了不算：它现在也接不了活。
func (s *Server) totalConcurrency(ctx context.Context) int {
	if s.Store == nil {
		return 1
	}
	accounts, err := s.Store.ListAccounts(ctx)
	if err != nil {
		return 1
	}
	exhausted, err := s.Store.ListExhausted(ctx)
	if err != nil {
		return 1
	}
	now := time.Now()
	usable := 0
	for _, a := range accounts {
		if a.Status != store.StatusActive {
			continue
		}
		if spentNow(exhausted, a.ID, now) {
			continue
		}
		usable++
	}
	return totalFor(usable, s.Cfg.Concurrency)
}

// spentNow says whether an account is marked as spent right now.
func spentNow(exhausted []store.Exhausted, accountID string, now time.Time) bool {
	for _, e := range exhausted {
		if e.AccountID != accountID {
			continue
		}
		if e.Until.After(now) {
			return true
		}
	}
	return false
}
