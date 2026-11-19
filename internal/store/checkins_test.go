package store_test

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/store"
)

// TestTheCheckinTimesAreTheOnesSinceAMoment says the check-in times
// since a moment are given, one per account.
//
// 两个签到可以是一天里的同一刻：日子分不开它们，时刻分得开。
//
// 它问的是「最近」：签到的积分七天就过期了，再往前的签到既不会让一个号
// 变成「待补」，也不会在十分钟的重试窗口里（规格 §5.3）。
func TestTheCheckinTimesAreTheOnesSinceAMoment(t *testing.T) {
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
	// 同一天再签一次是改那一行，不是插第二行：id 不变，时刻往前走。
	// 于是"id 最大的那条"不是最新的那条。
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_1", Day: "2026-11-14", Outcome: "claimed",
		Credit: 40, Streak: 1, CreatedAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	got, err := s.CheckinTimes(context.Background(), now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("the check-in times: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the check-in times = %d, want one per account", len(got))
	}
	at := map[string]time.Time{}
	for _, c := range got {
		at[c.AccountID] = c.At
	}
	// acc_1 的最新一次是后来改的那一行：它的 id 还是当初那个，时刻是最新
	// 的。
	if a := at["acc_1"].UnixMilli(); a != now.Add(time.Minute).UnixMilli() {
		t.Errorf("acc_1's check-in was at %v, want the newest one", a)
	}
	if a := at["acc_2"].UnixMilli(); a != now.UnixMilli() {
		t.Errorf("acc_2's check-in was at %v, want its own", a)
	}
	// 再往前的就不算了：问的是「最近」。
	none, err := s.CheckinTimes(context.Background(), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("the check-in times: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("the check-in times = %+v, want none", none)
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

// TestTheOldCheckinsAreThrownAway says the check-ins of the days before a
// day are thrown away.
//
// 签到的历史是唯一会长个没完的东西：一天一个号一行，1000 个号一年 36 万
// 行、40 MB。签到的积分七天就过期了（规格 §5.3），网页上每个号只摆最近
// 5 条 —— 再往前的记录除了占地方没有别的用处。
func TestTheOldCheckinsAreThrownAway(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	old := now.AddDate(0, 0, -40)
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_1", Day: store.DayKey(old), Outcome: "claimed",
		Credit: 10, CreatedAt: old,
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}
	if err := s.SaveCheckin(context.Background(), store.Checkin{
		AccountID: "acc_2", Day: store.DayKey(now), Outcome: "claimed",
		Credit: 20, CreatedAt: now,
	}); err != nil {
		t.Fatalf("save checkin: %v", err)
	}

	if err := s.PruneCheckins(context.Background(), now.AddDate(0, 0, -30)); err != nil {
		t.Fatalf("prune checkins: %v", err)
	}

	gone, err := s.ListCheckins(context.Background(), "acc_1", 0)
	if err != nil {
		t.Fatalf("list checkins: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("the old check-in is still there: %+v", gone)
	}
	kept, err := s.ListCheckins(context.Background(), "acc_2", 0)
	if err != nil {
		t.Fatalf("list checkins: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("the new check-in is gone: %+v", kept)
	}
}
