package usage

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/store"
)

// TestTheAccountsToReadAreTheOnesThatNeedIt says the read set is the
// accounts that need a reading, in the order they should be read.
//
// 该读的 = 从没读过的 ∪ 热的 ∪ 签到待补的 ∪ 标记到期的。没读过的排
// 在热的前面：它是一次性的活儿，排在后面就永远排在后面。
func TestTheAccountsToReadAreTheOnesThatNeedIt(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_old", now.Add(-time.Hour))
	mk(t, s, "acc_new", now.Add(-time.Minute))
	mk(t, s, "acc_never", time.Time{})
	mk(t, s, "acc_hot", now.Add(-time.Hour))
	mk(t, s, "acc_pending", now.Add(-time.Hour))
	mk(t, s, "acc_due", now.Add(-time.Hour))

	// 签到待补：签到比快照新，而且那次读没读到账本。
	if err := s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: "acc_pending", HasAmounts: false, CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_pending", Day: "2026-11-15", Outcome: "claimed",
		Credit: 10, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	// 标记到期：标记的时刻到了，账号回到轮转里，但没人读过它。
	if err := s.SetExhausted(context.Background(), "acc_due", "general",
		now.Add(-time.Minute), "quota"); err != nil {
		t.Fatalf("set exhausted: %v", err)
	}

	r := NewRound(s, nil, func() []string { return []string{"acc_hot"} }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	want := []string{"acc_pending", "acc_due", "acc_never", "acc_hot", "acc_old", "acc_new"}
	if len(got) != len(want) {
		t.Fatalf("the accounts to read = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the accounts to read = %v, want %v", got, want)
		}
	}
}

// TestTheAccountsToReadSkipTheSignedOut says a signed-out account is
// not read.
//
// 它读不到：读了也是白读。
func TestTheAccountsToReadSkipTheSignedOut(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	mk(t, s, "acc_out", time.Time{})
	if err := s.UpdateAccountStatus(context.Background(), "acc_out",
		store.StatusSignedOut, "the site refused the token", now); err != nil {
		t.Fatalf("sign out: %v", err)
	}
	r := NewRound(s, nil, func() []string { return nil }, config.Default())
	got, err := r.Targets(context.Background())
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the accounts to read = %v, want none", got)
	}
}

// newStore gives a store with the schema in it.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/mmxcode.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

// mk makes an account, with a reading of it or without one.
func mk(t *testing.T, s *store.Store, id string, readAt time.Time) {
	t.Helper()
	now := time.Now()
	if err := s.CreateAccount(context.Background(), store.Account{
		ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create account: %v", err)
	}
	if readAt.IsZero() {
		return
	}
	if err := s.SaveUsageSnapshot(context.Background(), store.UsageSnapshot{
		AccountID: id, Granted: 100, Consumed: 10, Remaining: 90, HasAmounts: true,
		CreatedAt: readAt,
	}); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
}
