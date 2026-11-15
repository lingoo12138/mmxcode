package store_test

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/store"
)

// TestTheLatestCheckinsOfEveryAccount says each account's newest
// check-in is the one that is given.
//
// 两个签到可以是一天里的同一刻：日子分不开它们，id 分得开。
func TestTheLatestCheckinsOfEveryAccount(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_1", Day: "2026-11-14", Outcome: "claimed",
		Credit: 10, Streak: 1, CreatedAt: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_1", Day: "2026-11-15", Outcome: "claimed",
		Credit: 20, Streak: 2, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_2", Day: "2026-11-15", Outcome: "claimed",
		Credit: 30, Streak: 1, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	got, err := s.LatestCheckins(context.Background())
	if err != nil {
		t.Fatalf("latest checkins: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the check-ins = %d, want one per account", len(got))
	}
	for _, c := range got {
		if c.AccountID == "acc_1" && c.Credit != 20 {
			t.Errorf("acc_1's check-in = %v, want the newest one", c.Credit)
		}
	}
}

// newStore gives a store with the schema in it.
//
// The two accounts the check-in test uses are in it as well: a check-in
// belongs to an account, and the store refuses a check-in whose
// account is not there.
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
	for _, id := range []string{"acc_1", "acc_2"} {
		now := time.Now()
		if err := s.CreateAccount(context.Background(), store.Account{
			ID: id, Name: id, Status: store.StatusActive,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create account: %v", err)
		}
	}
	return s
}
