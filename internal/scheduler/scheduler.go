// Package scheduler runs the recurring work: the daily check-in, the
// credits refresh, the token renewal and the outlet health check.
package scheduler

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"mmxcode/internal/checkin"
	"mmxcode/internal/config"
	"mmxcode/internal/manager"
	"mmxcode/internal/usage"
)

// egressTarget is the address the outlets are checked against.
//
// It is MiniMax's own agent host: a proxy that cannot reach it cannot
// carry a request to MiniMax either.
const egressTarget = "https://agent.minimax.io/"

// egressWriteTimeout is how long the health row may take to be
// written.
//
// The write is a small one, and it must not hold the scheduler up.
const egressWriteTimeout = 5 * time.Second

// Scheduler runs the recurring work.
type Scheduler struct {
	// Mgr holds the accounts.
	Mgr *manager.Manager
	// Cfg is the configuration.
	Cfg config.Config
	// target is the address the outlets are checked against. A test
	// points it at a stand-in, so that the check never leaves the
	// machine.
	target string
	// wg counts the goroutines the scheduler runs.
	//
	// 它是这一份进程的账：进程要关的时候，调度器的活儿也得停，而且要
	// 等在飞的那些停下来 —— 库是接着关的，它不该往一个正在关着的库上
	// 写。
	wg sync.WaitGroup
}

// New makes a scheduler.
func New(mgr *manager.Manager, cfg config.Config) *Scheduler {
	return &Scheduler{Mgr: mgr, Cfg: cfg, target: egressTarget}
}

// CheckinResult is one account's check-in.
type CheckinResult struct {
	// AccountID is the account.
	AccountID string
	// Outcome is what happened.
	Outcome string
	// Credit is what it paid.
	Credit float64
	// Streak is the days in a row.
	Streak int
	// Error is what went wrong, when something did.
	Error string
}

// CheckinDelay gives the wait before one account's check-in.
//
// The delay is random inside the window, so a hundred accounts do not
// all hit the site at the same second.
func CheckinDelay(base time.Duration, window time.Duration) time.Duration {
	if window <= 0 {
		return base
	}
	return base + time.Duration(rand.Int63n(int64(window)))
}

// NextCheckin gives the moment the daily check-in runs.
//
// The time is a wall-clock time in the local zone. When the moment has
// passed, it is tomorrow's.
func NextCheckin(now time.Time, at time.Time, jitter time.Duration) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Add(CheckinDelay(0, jitter))
}

// CheckinAll claims every account's daily credits.
func (s *Scheduler) CheckinAll(ctx context.Context) []CheckinResult {
	accounts, err := s.Mgr.Accounts(ctx)
	if err != nil {
		return nil
	}
	out := make([]CheckinResult, 0, len(accounts))
	checked := []string{}
	for _, a := range accounts {
		r := CheckinResult{AccountID: a.ID}
		res, err := s.Mgr.Checkin(ctx, a.ID)
		if err != nil {
			r.Error = err.Error()
		} else {
			r.Outcome = res.Outcome
			r.Credit = res.Credit
			r.Streak = res.Streak
			checked = append(checked, a.ID)
		}
		out = append(out, r)
	}
	// 签到给的积分是账本池里的：不读额度，卡上的余额还是签到前的旧
	// 数。签到的账号凑成一批，一次有预算地读；读到的数说有额度就清
	// 掉"用完"的标记。
	s.readCheckedIn(ctx, checked)
	return out
}

// readCheckedIn reads the credits of the accounts that were checked in.
//
// 一趟读完不完都没关系：没读到的那些在"签到待补"名单里，下一趟接着
// 读（待补排在最前面，所以几分钟内就清完了）。
func (s *Scheduler) readCheckedIn(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	round := s.round()
	res, err := round.Run(ctx, ids, round.Options())
	if err != nil {
		fmt.Printf("checkin: the credits could not be read: %v\n", err)
		return
	}
	for _, reason := range res.Reasons {
		fmt.Printf("checkin: %s\n", reason)
	}
	// 读到的数说话：说有额度就清掉"用完"的标记。
	for _, snap := range res.Read {
		if !usage.HasRoom(snap) {
			continue
		}
		if err := round.ClearSpent(ctx, snap.AccountID); err != nil {
			fmt.Printf("checkin %s: the spent mark could not be taken off: %v\n", snap.AccountID, err)
		}
	}
}

// The values of the pruning of the readings.
//
// 调度器每 usage.refresh.scan_every 扫一趟，每读一个账号就写一行读数。
// 不收拾的话，一天下来就是几千行 —— 而除了每个账号最新的一条（卡上摆的
// 就是它），剩下的谁也不看。
const (
	// pruneKeep is how many readings of every account are kept whatever
	// their age. One is the reading the card and the summary use: it is
	// the account's reading.
	pruneKeep = 1
	// pruneOlderThan is how long a reading that is not the newest one
	// is kept. A quarter of an hour is enough to see what just
	// happened; further back there is nothing to see.
	pruneOlderThan = 15 * time.Minute
)

// RefreshUsage reads the credits of the accounts that should be read.
//
// 它不再是"把所有账号读一遍"：账号多起来以后，冷账号的额度没有必要
// 读 —— 读了也是旧的。要读的是那些该读的：签到待补的、标记到期的、
// 从没读过的、还有热的。
func (s *Scheduler) RefreshUsage(ctx context.Context) {
	res, err := s.round().RefreshDue(ctx)
	if err != nil {
		fmt.Printf("usage: the round failed: %v\n", err)
		return
	}
	for _, reason := range res.Reasons {
		fmt.Printf("usage: %s\n", reason)
	}
	s.pruneReadings(ctx)
}

// pruneReadings throws away the readings that are not needed.
//
// 每个账号留最新的一条（不管它多老：卡上摆的就是它），其余的只留一刻
// 钟 —— 那个窗口是给「刚才发生了什么」看的，再往前的没有用。
func (s *Scheduler) pruneReadings(ctx context.Context) {
	if err := s.Mgr.Store.PruneUsageSnapshots(ctx, time.Now().Add(-pruneOlderThan), pruneKeep); err != nil {
		fmt.Printf("usage: the old readings could not be thrown away: %v\n", err)
	}
}

// round makes the reader of the credits.
//
// 它是网关和调度器共用的那一套：该读哪些账号、一趟读几个、并发几个。
func (s *Scheduler) round() *usage.Round {
	return usage.NewRound(s.Mgr.Store, s.Mgr, s.Mgr.HotAccounts, s.Cfg)
}

// RenewTokens renews the tokens that are about to end.
func (s *Scheduler) RenewTokens(ctx context.Context) {
	accounts, err := s.Mgr.Accounts(ctx)
	if err != nil {
		return
	}
	for _, a := range accounts {
		_, _ = s.Mgr.Token(ctx, a.ID)
	}
}

// Relogin signs in again the accounts that are signed out.
//
// It runs on its own clock: the cooldown is how long the scheduler
// waits between two rounds, so a sign-in that failed is not retried
// every minute.
func (s *Scheduler) Relogin(ctx context.Context) {
	res, err := s.Mgr.ReloginAll(ctx, s.Cfg.Batch.Relogin.MaxAccounts)
	if err != nil {
		// 这一趟根本没跑：说清楚，不然看着像「没什么要登的」。
		fmt.Printf("relogin: %v\n", err)
		return
	}
	if res.Fixed > 0 || res.Failed > 0 {
		fmt.Printf("relogin: %d fixed, %d failed\n", res.Fixed, res.Failed)
	}
	for _, r := range res.Reasons {
		fmt.Printf("relogin: %s\n", r)
	}
}

// CheckEgress tests every outlet and records the result.
//
// The probe and the write have a context each. The probe's timeout is
// over when the answer is in, and a write under that context would be
// thrown away before it reaches the database: the health table would
// stay empty and the WebUI would say "not checked" for ever.
func (s *Scheduler) CheckEgress(ctx context.Context) {
	target := s.target
	if target == "" {
		target = egressTarget
	}
	for name, url := range s.Cfg.Proxy.Groups {
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := s.Mgr.Egress.Check(probeCtx, url, target)
		cancel()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		// The write runs under a context of its own: the caller's may
		// be over already, and the health of the outlet has to be
		// written down all the same.
		writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), egressWriteTimeout)
		_ = s.Mgr.Store.SetEgressHealth(writeCtx, url, err == nil, msg)
		writeCancel()
		_ = name
	}
}

// usageEvery says how often the round of readings runs.
//
// 新键（usage.refresh.scan_every）是 0 的时候，间隔交回老的那个
// （schedule.usage_refresh）；再没有就是十五分钟。两个都写的时候新键
// 赢 —— 老的那个只是回退。
func (s *Scheduler) usageEvery() time.Duration {
	every := s.Cfg.Usage.Refresh.ScanEvery.Duration()
	if every <= 0 {
		every = s.Cfg.Schedule.UsageRefresh.Duration()
	}
	if every <= 0 {
		every = 15 * time.Minute
	}
	return every
}

// Run runs the recurring work until the context ends.
func (s *Scheduler) Run(ctx context.Context) {
	at, err := time.Parse("15:04", s.Cfg.Schedule.CheckinAt)
	if err != nil {
		at = time.Date(0, 1, 1, 9, 0, 0, 0, time.Local)
	}
	usageEvery := s.usageEvery()
	tokenEvery := s.Cfg.Schedule.TokenRefresh.Duration()
	if tokenEvery <= 0 {
		tokenEvery = 5 * time.Minute
	}
	egressEvery := s.Cfg.Schedule.EgressHealth.Duration()
	if egressEvery <= 0 {
		egressEvery = 2 * time.Minute
	}

	s.run(ctx, usageEvery, s.RefreshUsage)
	s.run(ctx, tokenEvery, s.RenewTokens)
	s.run(ctx, egressEvery, s.CheckEgress)

	// 重登是单独的活儿，不在续期器那条热路径上：续期器照旧把链断的
	// 账号标成 signed_out，签回来的活儿交给这里。
	if s.Cfg.Batch.Relogin.Enabled {
		every := s.Cfg.Batch.Relogin.Cooldown.Duration()
		if every <= 0 {
			every = time.Hour
		}
		s.run(ctx, every, s.Relogin)
	}
	for {
		next := NextCheckin(time.Now(), at, 30*time.Minute)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			for _, r := range s.CheckinAll(ctx) {
				if r.Error != "" {
					fmt.Printf("checkin %s: %s\n", r.AccountID, r.Error)
					continue
				}
				fmt.Printf("checkin %s: %s\n", r.AccountID, r.Outcome)
			}
		}
	}
}

// run runs a piece of work every so often, on the scheduler's account.
//
// 它算在调度器的账上：进程要关的时候要等它停下来（Wait）。
func (s *Scheduler) run(ctx context.Context, every time.Duration, work func(context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop(ctx, every, work)
	}()
}

// Wait waits for the scheduler's work to stop.
//
// 调度器的活儿挂在它自己的上下文上：那个上下文一结束，活儿就不该再开
// 新的一趟，而在飞的那一趟要等它自己停下（或者放弃）。
//
// 它等的是那些周期活儿。Run 自己是在调用它的那条线上的：等 Run 回来
// 之后调 Wait，才是「全都停了」。
func (s *Scheduler) Wait() {
	s.wg.Wait()
}

// loop runs a piece of work every so often.
func (s *Scheduler) loop(ctx context.Context, every time.Duration, work func(context.Context)) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			work(ctx)
		}
	}
}

// OutcomeNames gives the outcome names, for the WebUI.
func OutcomeNames() []string {
	return []string{
		checkin.OutcomeClaimed,
		checkin.OutcomeDone,
		checkin.OutcomeIneligible,
		checkin.OutcomeInactive,
		checkin.OutcomeFailed,
	}
}
