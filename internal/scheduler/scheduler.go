// Package scheduler runs the recurring work: the daily check-in, the
// credits refresh, the token renewal and the outlet health check.
package scheduler

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"mmxcode/internal/checkin"
	"mmxcode/internal/config"
	"mmxcode/internal/manager"
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
	for _, a := range accounts {
		r := CheckinResult{AccountID: a.ID}
		res, err := s.Mgr.Checkin(ctx, a.ID)
		if err != nil {
			r.Error = err.Error()
		} else {
			r.Outcome = res.Outcome
			r.Credit = res.Credit
			r.Streak = res.Streak
		}
		out = append(out, r)
	}
	return out
}

// RefreshUsage reads every account's credits.
func (s *Scheduler) RefreshUsage(ctx context.Context) {
	accounts, err := s.Mgr.Accounts(ctx)
	if err != nil {
		return
	}
	for _, a := range accounts {
		if _, err := s.Mgr.RefreshUsage(ctx, a.ID); err != nil {
			// 一个账号读不到不影响别的
			continue
		}
	}
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
	fixed, failed := s.Mgr.ReloginAll(ctx, s.Cfg.Batch.Relogin.MaxAccounts)
	if fixed > 0 || failed > 0 {
		fmt.Printf("relogin: %d fixed, %d failed\n", fixed, failed)
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

// Run runs the recurring work until the context ends.
func (s *Scheduler) Run(ctx context.Context) {
	at, err := time.Parse("15:04", s.Cfg.Schedule.CheckinAt)
	if err != nil {
		at = time.Date(0, 1, 1, 9, 0, 0, 0, time.Local)
	}
	usageEvery := s.Cfg.Schedule.UsageRefresh.Duration()
	if usageEvery <= 0 {
		usageEvery = 15 * time.Minute
	}
	tokenEvery := s.Cfg.Schedule.TokenRefresh.Duration()
	if tokenEvery <= 0 {
		tokenEvery = 5 * time.Minute
	}
	egressEvery := s.Cfg.Schedule.EgressHealth.Duration()
	if egressEvery <= 0 {
		egressEvery = 2 * time.Minute
	}

	go s.loop(ctx, usageEvery, s.RefreshUsage)
	go s.loop(ctx, tokenEvery, s.RenewTokens)
	go s.loop(ctx, egressEvery, s.CheckEgress)

	// 重登是单独的活儿，不在续期器那条热路径上：续期器照旧把链断的
	// 账号标成 signed_out，签回来的活儿交给这里。
	if s.Cfg.Batch.Relogin.Enabled {
		every := s.Cfg.Batch.Relogin.Cooldown.Duration()
		if every <= 0 {
			every = time.Hour
		}
		go s.loop(ctx, every, s.Relogin)
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
