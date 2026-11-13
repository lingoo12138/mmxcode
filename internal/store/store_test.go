package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAccountRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	a := Account{
		ID:         "acc_01",
		Name:       "主号",
		UserID:     "1234567890",
		Email:      "a@example.com",
		Status:     StatusActive,
		ProxyGroup: "warp",
		ProxyURL:   "",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if err := s.CreateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "a@example.com" || got.ProxyGroup != "warp" || got.Status != StatusActive {
		t.Fatalf("got %+v", got)
	}

	a.Name = "改名了"
	a.ProxyURL = "http://127.0.0.1:1080"
	if err := s.UpdateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAccount(ctx, "acc_01")
	if got.Name != "改名了" || got.ProxyURL != "http://127.0.0.1:1080" {
		t.Fatalf("after update got %+v", got)
	}

	list, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("list = %d accounts", len(list))
	}

	if err := s.DeleteAccount(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetAccount(ctx, "acc_01"); err == nil {
		t.Fatal("expected the account to be gone")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	exp := time.Now().Add(time.Hour)
	if err := s.SaveToken(ctx, Token{
		AccountID: "acc_01", Access: "a", Refresh: "r",
		ExpiresAt: exp, Subject: "sub1", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Access != "a" || got.Refresh != "r" || got.Subject != "sub1" {
		t.Fatalf("got %+v", got)
	}
	if d := got.ExpiresAt.Sub(exp); d > time.Second || d < -time.Second {
		t.Fatalf("expires_at drifted by %v", d)
	}
}

func TestCheckinUniquePerDay(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	c := Checkin{AccountID: "acc_01", Day: "2026-11-13", Outcome: "claimed", Credit: 25, Streak: 3}
	if err := s.SaveCheckin(ctx, c); err != nil {
		t.Fatal(err)
	}
	// 同一天再存一次是更新，不是插入第二行
	c.Outcome = "done"
	if err := s.SaveCheckin(ctx, c); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Outcome != "done" {
		t.Fatalf("got %+v", list)
	}
}

func TestExhaustedWindows(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	until := time.Now().Add(time.Hour)
	if err := s.SetExhausted(ctx, "acc_01", "general", until, "quota"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].WindowKey != "general" || got[0].Reason != "quota" {
		t.Fatalf("got %+v", got)
	}
	if err := s.ClearExhausted(ctx, "acc_01", "general"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListExhausted(ctx)
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestSessionBinding(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.BindSession(ctx, "sess-1", "acc_01"); err != nil {
		t.Fatal(err)
	}
	id, ok, err := s.SessionAccount(ctx, "sess-1")
	if err != nil || !ok || id != "acc_01" {
		t.Fatalf("got %q %v %v", id, ok, err)
	}
	// 改绑
	if err := s.BindSession(ctx, "sess-1", "acc_02"); err != nil {
		t.Fatal(err)
	}
	id, _, _ = s.SessionAccount(ctx, "sess-1")
	if id != "acc_02" {
		t.Fatalf("got %q", id)
	}
}

func TestUsageSnapshot(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", Granted: 600, Consumed: 120.5, Remaining: 479.5,
		HasAmounts: true,
		ExpiresAt:  time.Now().Add(time.Hour), Plan: "M Plan",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Remaining != 479.5 {
		t.Fatalf("got %+v", got)
	}
	if !got[0].HasAmounts {
		t.Fatal("the amounts were lost")
	}
}

func TestUsageSnapshotWithoutAmounts(t *testing.T) {
	// 读不到账本和读到 0 是两回事：读不到就没有数
	ctx := context.Background()
	s := openTest(t)
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_02", Plan: "M Plan", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if got[0].HasAmounts {
		t.Fatalf("a reading with no amounts says it has amounts: %+v", got[0])
	}
}

// TestLatestUsageSnapshotIsOneRowPerAccount says two readings of one
// account in the same millisecond come back as one row.
//
// 两行都回来的时候求和会把同一个账号的积分算两遍。
func TestLatestUsageSnapshotIsOneRowPerAccount(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	// 同一毫秒里的两次刷新
	at := time.UnixMilli(1763000000000)
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 100, Consumed: 0, Remaining: 100,
		ExpiresAt: at.Add(time.Hour), CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true,
		Granted: 300, Consumed: 0, Remaining: 300,
		ExpiresAt: at.Add(time.Hour), CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want one per account", len(got))
	}
	if got[0].Remaining != 300 {
		t.Fatalf("got %+v, want the newest reading", got[0])
	}
}

// TestLatestUsageSnapshotGivesOneAccount says one account's newest
// reading is asked for on its own.
func TestLatestUsageSnapshotGivesOneAccount(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now()
	for _, id := range []string{"acc_01", "acc_02"} {
		if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
			AccountID: id, HasAmounts: true, Granted: 10, Remaining: 10, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_02")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acc_02" {
		t.Fatalf("got %+v, want acc_02", got)
	}
	// 没有读过的账号没有读数，也没有错
	none, err := s.LatestUsageSnapshot(ctx, "acc_99")
	if err != nil {
		t.Fatal(err)
	}
	if none.AccountID != "" {
		t.Fatalf("got %+v, want nothing", none)
	}
}

// TestUsageSnapshotKeepsTheReadErrors says the errors of the parts
// that could not be read are written down with the reading.
//
// 读不到窗口和没有窗口不是一回事，读数要能说出来。
func TestUsageSnapshotKeepsTheReadErrors(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true, Granted: 10, Remaining: 10,
		MembershipError: "the membership is not there",
		WindowsError:    "the windows are not there",
		CreatedAt:       time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.MembershipError != "the membership is not there" {
		t.Fatalf("got %+v", got)
	}
	if got.WindowsError != "the windows are not there" {
		t.Fatalf("got %+v", got)
	}
}

// TestDayKeyIsTheSitesDay says the day key is the site's day.
//
// The site counts its days in China: a check-in at half past midnight
// in that zone is the next day, whatever the machine's own zone is.
func TestDayKeyIsTheSitesDay(t *testing.T) {
	// 2026-11-13 20:00 UTC is 2026-11-14 04:00 in China
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	if got := DayKey(at); got != "2026-11-14" {
		t.Fatalf("day = %q, want 2026-11-14", got)
	}
}
