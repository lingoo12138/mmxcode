package gateway

import (
	"context"
	"log"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/pool"
	"mmxcode/internal/store"
)

// poolReadBudget gives how many accounts one filling pass may read the
// credits of.
//
// 配 0 的时候按 1 算，跟读额度那一趟是同一条规矩（usage.batch）：0 个一
// 批是一趟也读不了，那不是关掉，是坏了。
//
// keeper 那边的 0 说的是"不限"，所以这一层必须先把它夹住：不夹住的话，
// 一个写 `usage.refresh.batch: 0` 的人把补号那一趟的预算打开了 —— 一趟
// 把候选表读一遍，一次读是 2~11 个上游请求（规格 §6.4）。
func poolReadBudget(cfg config.Usage) int {
	if cfg.Refresh.Batch <= 0 {
		return 1
	}
	return cfg.Refresh.Batch
}

// poolSource gives the pool the accounts it could take.
type poolSource struct {
	s *store.Store
	// window is how long a session counts as active.
	//
	// 它是「这段对话还在用吗」的那把尺子：这段时间里被碰过的对话，它绑
	// 着的号要留在池里（规格不变量 3）。
	window time.Duration
}

// Candidates gives the accounts that are not in the pool, in the order
// they should go in.
func (p poolSource) Candidates(ctx context.Context, limit int, exclude map[string]bool) ([]store.Candidate, error) {
	// 没有窗口就是没有「活跃」：没有对话算活跃，一个号也不因为对话而
	// 排在前面（规格 §6.3 的那一项直接不算）。
	since := time.Now()
	if p.window > 0 {
		since = time.Now().Add(-p.window)
	}
	return p.s.PoolCandidates(ctx, limit, exclude, since)
}

// poolTarget says how many accounts the pool should hold.
//
// 池多大 = max(总并发, 活跃会话数) × 倍数（规格 §6.3）：
//
//   - 总并发是「该跑多少个请求」= 可用账号数 ÷ concurrency.total_ratio
//     （小规模至少 concurrency.total_min 个）；
//   - 活跃会话数是「有多少段对话在用」—— 会话钉在号上，钉住的号不在池
//     里会被改绑，缓存命中就没了，所以池要装得下它们；
//   - 倍数是 2：再留一倍给补号（补号要读一次额度，那段时间位置空着）。
//
// 上限有三个：配的 max_size、内存预算算出来的数、还有库里能用的号数。
func (s *Server) poolTarget(ctx context.Context) int {
	factor := s.Cfg.Pool.Factor
	if factor < 1 {
		factor = 1
	}
	usable, err := s.Store.CountUsable(ctx)
	if err != nil {
		return 0
	}
	demand := totalFor(usable, s.Cfg.Concurrency)
	if w := s.Cfg.Pool.SessionWindow.Duration(); w > 0 {
		n, err := s.Store.CountRecentSessions(ctx, time.Now().Add(-w))
		if err == nil && n > demand {
			demand = n
		}
	}
	want := demand * factor
	if s.Cfg.Pool.MaxSize > 0 && want > s.Cfg.Pool.MaxSize {
		want = s.Cfg.Pool.MaxSize
	}
	if mb := s.Cfg.Pool.MemoryBudgetMB; mb > 0 {
		byMemory := mb * 1024 * 1024 / pool.BytesPerAccount
		if want > byMemory {
			want = byMemory
		}
	}
	// 池装不下比库里能用的号更多的东西。
	if want > usable {
		want = usable
	}
	return want
}

// SeedPool fills the pool with what the database knows.
//
// 它**不问站点**：读额度是 2~11 个上游请求，填满 400 个号要半个多小时，
// 而网关要立刻能用（规格 §6.3）。旧数只是高估 —— 撞空的时候换号重试会
// 把标记写准，自愈。
func (s *Server) SeedPool(ctx context.Context) {
	want := s.poolTarget(ctx)
	s.Pool.SetCapacity(want)
	n, err := s.Keeper.Seed(ctx, want)
	if err != nil {
		log.Printf("the account pool could not be filled: %v", err)
		return
	}
	log.Printf("the account pool has %d of %d accounts", n, want)
}

// TopUpPool puts accounts into the pool until it holds what it should.
//
// 它不挡客户端：后台的活。
//
// 补进来的号读一次额度（read_on_admit）：读到的数说它有额度，它才进池；
// 读到「用完了」，它不进池，而且被标记掉。
func (s *Server) TopUpPool(ctx context.Context) {
	want := s.poolTarget(ctx)
	s.Pool.SetCapacity(want)
	n, err := s.Keeper.Replenish(ctx, want)
	if err != nil {
		log.Printf("the account pool could not be filled: %v", err)
		return
	}
	if n > 0 {
		log.Printf("%d accounts went into the pool (%d of %d)", n, s.Pool.Len(), want)
	}
}

// poolLoop keeps the pool full.
//
// 每 scan_every 一趟：池里少了号就补上。补号是上游的活，所以它在后台，
// 而且一次只跑一趟（规格 §6.4）。
//
// 间隔跟读额度那一趟是同一把尺子（usage.refresh.scan_every，0 就交回
// schedule.usage_refresh）：写 0 的人说的是「别那么频繁」，不是「每 30 秒
// 一趟」。
func (s *Server) poolLoop() {
	every := s.Cfg.Usage.Refresh.ScanEvery.Duration()
	if every <= 0 {
		every = s.Cfg.Schedule.UsageRefresh.Duration()
	}
	if every <= 0 {
		every = 15 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.lifeCtx.Done():
			return
		case <-t.C:
			s.startBackground(func() { s.TopUpPool(s.lifeCtx) })
		}
	}
}

// dropDeadAccount takes an account that cannot sign in out of the pool.
//
// 池里留着一个签不上的号有两个坏处：它占着位置（承诺的并发按池夹，池里
// 少一个能答的号就少承诺一个），而且挑号会挑到它 —— 每挑一次就是一次
// 401 加一次续签。
//
// 库里的状态说了算：还是 active 的就是暂时的（续签抖了一下），那就不动
// 它 —— 签不上的号总归会再来一次，那一次再看。
func (s *Server) dropDeadAccount(ctx context.Context, accountID string) {
	if s.Pool == nil || s.Store == nil {
		return
	}
	acc, err := s.Store.GetAccount(ctx, accountID)
	if err != nil {
		return
	}
	if acc.Status != store.StatusSignedOut {
		return
	}
	s.Pool.Remove(accountID)
}
