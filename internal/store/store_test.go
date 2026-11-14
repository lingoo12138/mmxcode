package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
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

// makeAccount puts an account in the store.
//
// The children say which account they belong to, so a child without an
// account is refused.
func makeAccount(t *testing.T, s *Store, id string) {
	t.Helper()
	err := s.CreateAccount(context.Background(), Account{
		ID: id, Name: id, Status: StatusActive, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
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
	got, err = s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
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

// TestNotFoundIsOneSentinel says every way of saying "there is no
// such row" says it the same way.
//
// 找不到就是找不到，一种说法就够。
func TestNotFoundIsOneSentinel(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	if _, err := s.GetAccount(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get account: %v, want ErrNotFound", err)
	}
	if _, err := s.GetToken(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get token: %v, want ErrNotFound", err)
	}
	err := s.UpdateAccount(ctx, Account{ID: "nope", Name: "x", Status: StatusActive})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("update account: %v, want ErrNotFound", err)
	}
	err = s.DeleteAccount(ctx, "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete account: %v, want ErrNotFound", err)
	}
	// 哨兵不能是 database/sql 那个
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("the error is the database's own sentinel")
	}
}

// TestDeleteAccountTakesTheChildrenWithIt says an account's children
// go with the account.
//
// 删除账号要带着它的账本、签到、用量和会话一起走。
func TestDeleteAccountTakesTheChildrenWithIt(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.SaveToken(ctx, Token{AccountID: "acc_01", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetExhausted(ctx, "acc_01", "general", time.Now().Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckin(ctx, Checkin{AccountID: "acc_01", Day: "2026-11-13",
		Outcome: "claimed", Credit: 25, Streak: 3, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "acc_01", HasAmounts: true,
		Granted: 100, Remaining: 100, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "sess-1", "acc_01"); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteAccount(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"tokens", "exhausted_windows", "checkins", "usage_snapshots", "sessions",
	} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s has %d rows left, want none", table, n)
		}
	}
}

// TestChildRowWithoutAnAccountIsRefused says a child row of an
// account that is not there is not accepted.
//
// 没有账号的孩子不算数。
func TestChildRowWithoutAnAccountIsRefused(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	err := s.SaveToken(ctx, Token{AccountID: "nope", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()})
	if err == nil {
		t.Fatal("the token of an account that is not there was accepted")
	}
	err = s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "nope", HasAmounts: true,
		Granted: 1, Remaining: 1, CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("the reading of an account that is not there was accepted")
	}
	err = s.BindSession(ctx, "sess-1", "nope")
	if err == nil {
		t.Fatal("the session of an account that is not there was accepted")
	}
	err = s.SaveCheckin(ctx, Checkin{AccountID: "nope", Day: "2026-11-13",
		Outcome: "claimed", CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("the check-in of an account that is not there was accepted")
	}
	err = s.SetExhausted(ctx, "nope", "general", time.Now().Add(time.Hour), "quota")
	if err == nil {
		t.Fatal("the window of an account that is not there was accepted")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
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

// TestANullSubjectIsReadable says a token whose subject was never
// written can still be read.
//
// 老库里的 subject 可以是空的，空的也要读得出来。
func TestANullSubjectIsReadable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	makeOldFile(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "" {
		t.Fatalf("subject = %q, want nothing", got.Subject)
	}
	if got.Access != "a" {
		t.Fatalf("got %+v", got)
	}
}

func TestCheckinUniquePerDay(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
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

// TestANullCreditIsReadable says a check-in whose credit was never
// written can still be read.
func TestANullCreditIsReadable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	makeOldFile(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %+v", list)
	}
	if list[0].Credit != 0 || list[0].Streak != 0 {
		t.Fatalf("got %+v", list[0])
	}
}

// TestListCheckinsLimit says a limit of zero is no limit.
func TestListCheckinsLimit(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	for _, day := range []string{"2026-11-11", "2026-11-12", "2026-11-13"} {
		if err := s.SaveCheckin(ctx, Checkin{AccountID: "acc_01", Day: day,
			Outcome: "claimed", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListCheckins(ctx, "acc_01", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d check-ins, want all three", len(list))
	}
	list, err = s.ListCheckins(ctx, "acc_01", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d check-ins, want two", len(list))
	}
}

func TestExhaustedWindows(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
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
	got, err = s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// TestTheWindowsThatHaveComeBackAreStillThere says ListExhausted
// gives the windows that have run out as well.
//
// 到期的窗口还在表里，谁在用自己看着办。
func TestTheWindowsThatHaveComeBackAreStillThere(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	now := time.Now()
	if err := s.SetExhausted(ctx, "acc_01", "general", now.Add(-time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v, want the expired window too", got)
	}
	if err := s.PruneExpiredWindows(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListExhausted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want the expired window gone", got)
	}
}

// TestPruneUsageSnapshots says the old readings are thrown away and
// the newest ones are kept.
//
// 旧读数扔掉，新的留下。
func TestPruneUsageSnapshots(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	old := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 5; i++ {
		if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "acc_01", HasAmounts: true,
			Granted: float64(i), Remaining: float64(i), CreatedAt: old.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	// 一条新的
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{AccountID: "acc_01", HasAmounts: true,
		Granted: 9, Remaining: 9, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	if err := s.PruneUsageSnapshots(ctx, time.Now().Add(-24*time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_snapshots").Scan(&n); err != nil {
		t.Fatal(err)
	}
	// 每个账号留最新的一条
	if n != 1 {
		t.Fatalf("got %d readings, want the newest one", n)
	}
	// 一条都不留的时候就没有了
	if err := s.PruneUsageSnapshots(ctx, time.Now().Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM usage_snapshots").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("got %d readings, want none", n)
	}
}

func TestSessionBinding(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
	makeAccount(t, s, "acc_02")
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

// TestUpstreamIDIsRandomAndKept says every session has an upstream id
// of its own, and keeps it.
//
// 两段一样的开场白不能共用一个上游会话。
func TestUpstreamIDIsRandomAndKept(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	first, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("the session has no upstream id")
	}
	again, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("got %q and %q, want the same id", again, first)
	}
	other, err := s.UpstreamID(ctx, "sess-2")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("two sessions got the same upstream id")
	}
	// 同一个 key 再问还是那个 id
	third, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatalf("got %q, want %q", third, first)
	}
}

// TestTheUpstreamIDSurvivesTheBinding says the upstream id belongs
// to the conversation and not to the account.
func TestTheUpstreamIDSurvivesTheBinding(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	id, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "sess-1", "acc_01"); err != nil {
		t.Fatal(err)
	}
	again, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("got %q, want %q", again, id)
	}
	acc, ok, err := s.SessionAccount(ctx, "sess-1")
	if err != nil || !ok || acc != "acc_01" {
		t.Fatalf("got %q %v %v", acc, ok, err)
	}
}

func TestUsageSnapshot(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
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
	makeAccount(t, s, "acc_02")
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

// TestTheAmountsAreKeptWithoutTheFlag says a caller that forgets to
// say that a reading has amounts does not lose the numbers.
//
// 忘了说有没有数，数还在。
func TestTheAmountsAreKeptWithoutTheFlag(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", Granted: 100, Consumed: 10, Remaining: 90,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasAmounts {
		t.Fatalf("the numbers were lost: %+v", got)
	}
	if got.Granted != 100 || got.Consumed != 10 || got.Remaining != 90 {
		t.Fatalf("got %+v", got)
	}
}

// TestLatestUsageSnapshotIsOneRowPerAccount says two readings of one
// account in the same millisecond come back as one row.
//
// 两行都回来的时候求和会把同一个账号的积分算两遍。
func TestLatestUsageSnapshotIsOneRowPerAccount(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")
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
		makeAccount(t, s, id)
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
	makeAccount(t, s, "acc_01")
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

// TestTheTimesComeBackInUTC says a time that was written in UTC comes
// back in UTC.
//
// 写进去是 UTC，读出来也得是 UTC。
func TestTheTimesComeBackInUTC(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	makeAccount(t, s, "acc_01")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true, Granted: 1, Remaining: 1, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at is in %v, want UTC", got.CreatedAt.Location())
	}
	if !got.CreatedAt.Equal(at) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, at)
	}
}

// TestTheStoreHasAClock says the store's clock is what a row is
// written at when the caller does not say.
//
// 调用方不说是几点，存储层自己说。
func TestTheStoreHasAClock(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	s.Clock = func() time.Time { return at }
	makeAccount(t, s, "acc_01")

	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", HasAmounts: true, Granted: 1, Remaining: 1,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LatestUsageSnapshot(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(at) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, at)
	}
}

// TestAnUnknownStatusIsRefused says an account cannot have a status
// nobody wrote.
//
// 写错的状态会让账号悄悄不能用。
func TestAnUnknownStatusIsRefused(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	err := s.CreateAccount(ctx, Account{ID: "acc_01", Name: "a", Status: "actve"})
	if err == nil {
		t.Fatal("the account with the misspelled status was accepted")
	}
	makeAccount(t, s, "acc_02")
	err = s.UpdateAccount(ctx, Account{ID: "acc_02", Name: "a", Status: "actve"})
	if err == nil {
		t.Fatal("the account with the misspelled status was accepted")
	}
	err = s.UpdateAccountStatus(ctx, "acc_02", "actve", "", time.Now())
	if err == nil {
		t.Fatal("the account with the misspelled status was accepted")
	}
}

// TestTheNarrowSetters says the state and the outlet are written
// without reading the account first.
func TestTheNarrowSetters(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.UpdateAccountStatus(ctx, "acc_01", StatusSignedOut, "the sign-in was refused", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSignedOut || got.LastError != "the sign-in was refused" {
		t.Fatalf("got %+v", got)
	}
	if err := s.SetAccountProxy(ctx, "acc_01", "warp", "http://127.0.0.1:1080", time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyGroup != "warp" || got.ProxyURL != "http://127.0.0.1:1080" {
		t.Fatalf("got %+v", got)
	}
	// 没有的账号还是找不到
	if err := s.UpdateAccountStatus(ctx, "nope", StatusActive, "", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update the status: %v, want ErrNotFound", err)
	}
	if err := s.SetAccountProxy(ctx, "nope", "warp", "", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set the outlet: %v, want ErrNotFound", err)
	}
}

// TestThePragmasAreOnEveryConnection says every connection of the
// pool has the pragmas, and not only the first one.
//
// 每个连接都得带着 pragma，不能只有第一个。
func TestThePragmasAreOnEveryConnection(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	conns := make([]*sql.Conn, 0, maxConns)
	for i := 0; i < maxConns; i++ {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i, c := range conns {
		var fk int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Fatalf("connection %d has foreign_keys = %d, want 1", i, fk)
		}
		var busy int
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if busy != 5000 {
			t.Fatalf("connection %d has busy_timeout = %d, want 5000", i, busy)
		}
		var mode string
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" {
			t.Fatalf("connection %d has journal_mode = %q, want wal", i, mode)
		}
	}
}

// TestOneStoreIsUsedFromSeveralGoroutines says the store can be used
// by several goroutines at once.
//
// 几个线程一起用也不该打架。
func TestOneStoreIsUsedFromSeveralGoroutines(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("acc_%02d", i)
			if err := s.CreateAccount(ctx, Account{ID: id, Name: id, Status: StatusActive}); err != nil {
				errs <- err
				return
			}
			for j := 0; j < 5; j++ {
				if _, err := s.GetAccount(ctx, id); err != nil {
					errs <- err
					return
				}
				if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
					AccountID: id, HasAmounts: true, Granted: 1, Remaining: 1,
					CreatedAt: time.Now(),
				}); err != nil {
					errs <- err
					return
				}
				if _, err := s.LatestUsageSnapshots(ctx); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("the store was used from several goroutines: %v", err)
	}
}

// TestThePathWithAQuestionMarkIsThePath says a path with a question
// mark in it is the path and not the end of the file name.
//
// 路径里有问号，问号不能把文件名截断。
func TestThePathWithAQuestionMarkIsThePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a?b&c#d%e.db")

	d := dsn(path)
	if !strings.HasPrefix(d, "file:") {
		t.Fatalf("dsn = %q, want a file: URI", d)
	}
	if !strings.Contains(d, "%3F") {
		t.Fatalf("dsn = %q, want the question mark escaped", d)
	}
	if !strings.Contains(d, "%26") || !strings.Contains(d, "%23") || !strings.Contains(d, "%25") {
		t.Fatalf("dsn = %q, want the other marks escaped", d)
	}
	if !strings.Contains(d, "_pragma=foreign_keys(1)") {
		t.Fatalf("dsn = %q, want the pragmas", d)
	}

	if runtime.GOOS == "windows" {
		// Windows does not allow a question mark in a file name, so
		// the file itself cannot be made there.
		t.Skip("windows does not allow a question mark in a file name")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the database is not at the path: %v", err)
	}
	var fk int
	if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}
}

// TestTheFileIsOnlyForItsOwner says the database file is not for
// everybody.
//
// 数据库里有明文的 refresh token，别人不能读。
//
// 目录是存储层自己建的那一个：它建成只有属主能进的样子。已经在那儿
// 的目录不归它管 —— 把用户自己挑的目录改成 700 是它不该做的事。文件
// 不一样：不管是谁建的，存储层都把它改成 600。
func TestTheFileIsOnlyForItsOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no such a mode")
	}
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "data.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the file's mode is %o, want 600", mode)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := di.Mode().Perm(); mode != 0o700 {
		t.Fatalf("the directory's mode is %o, want 700", mode)
	}
	// WAL 和 SHM 里也是明文：SQLite 照着数据库文件的权限建它们。
	for _, name := range []string{path + "-wal", path + "-shm"} {
		wi, err := os.Stat(name)
		if err != nil {
			continue
		}
		if mode := wi.Mode().Perm(); mode != 0o600 {
			t.Fatalf("%s's mode is %o, want 600", filepath.Base(name), mode)
		}
	}
}

// oldSchema is the schema of a file that was made before the
// versions were kept.
const oldSchema = `
CREATE TABLE accounts (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL,
  user_id          TEXT,
  email            TEXT,
  status           TEXT NOT NULL,
  proxy_group      TEXT,
  proxy_url        TEXT,
  pending_persist  INTEGER NOT NULL DEFAULT 0,
  exhausted_until  INTEGER,
  last_error       TEXT,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);
CREATE TABLE tokens (
  account_id  TEXT PRIMARY KEY,
  access      TEXT NOT NULL,
  refresh     TEXT NOT NULL,
  expires_at  INTEGER NOT NULL,
  subject     TEXT,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE exhausted_windows (
  account_id TEXT NOT NULL,
  window_key TEXT NOT NULL,
  until      INTEGER NOT NULL,
  reason     TEXT,
  PRIMARY KEY (account_id, window_key)
);
CREATE TABLE sessions (
  session_key TEXT PRIMARY KEY,
  account_id  TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE checkins (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  day        TEXT NOT NULL,
  outcome    TEXT NOT NULL,
  credit     REAL,
  streak     INTEGER,
  message    TEXT,
  created_at INTEGER NOT NULL,
  UNIQUE (account_id, day)
);
CREATE TABLE usage_snapshots (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id  TEXT NOT NULL,
  granted     REAL,
  consumed    REAL,
  remaining   REAL,
  expires_at  INTEGER,
  plan        TEXT,
  plan_until  INTEGER,
  windows_json TEXT,
  balance     TEXT,
  created_at  INTEGER NOT NULL
);
CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE egress_health (
  url        TEXT PRIMARY KEY,
  ok         INTEGER NOT NULL,
  error      TEXT,
  checked_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_snapshots(account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_checkins_account ON checkins(account_id, day DESC);
`

// makeOldFile writes a file in the shape the code used to make.
func makeOldFile(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO accounts (id,name,user_id,email,status,proxy_group,proxy_url,pending_persist,exhausted_until,last_error,created_at,updated_at)
		  VALUES ('acc_01','old','123','a@example.com','active','warp','',1,NULL,'',?,?)`, []any{now, now}},
		{`INSERT INTO tokens (account_id,access,refresh,expires_at,subject,updated_at) VALUES ('acc_01','a','r',?,NULL,?)`, []any{now, now}},
		{`INSERT INTO checkins (account_id,day,outcome,credit,streak,message,created_at) VALUES ('acc_01','2026-11-13','claimed',NULL,NULL,'',?)`, []any{now}},
		{`INSERT INTO usage_snapshots (account_id,granted,consumed,remaining,created_at) VALUES ('acc_01',600,100,500,?)`, []any{now}},
		{`INSERT INTO sessions (session_key,account_id,updated_at) VALUES ('sess-1','acc_01',?)`, []any{now}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
}

// TestTheOldFileIsBroughtForward says a file that was made by an
// older version is brought to the shape the code wants, with its
// rows still in it.
//
// 老库要能打开，行还要在。
func TestTheOldFileIsBroughtForward(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	makeOldFile(t, path)

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("the file is at version %d, want %d", version, schemaVersion)
	}

	// 行还在
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "old" || a.ProxyGroup != "warp" {
		t.Fatalf("got %+v", a)
	}
	tok, err := s.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "a" || tok.Subject != "" {
		t.Fatalf("got %+v", tok)
	}
	list, err := s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Credit != 0 {
		t.Fatalf("got %+v", list)
	}
	snaps, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Remaining != 500 {
		t.Fatalf("got %+v", snaps)
	}
	acc, ok, err := s.SessionAccount(ctx, "sess-1")
	if err != nil || !ok || acc != "acc_01" {
		t.Fatalf("got %q %v %v", acc, ok, err)
	}

	// 新列有了
	for _, c := range []struct{ table, column string }{
		{"usage_snapshots", "membership_error"},
		{"usage_snapshots", "windows_error"},
		{"sessions", "upstream_id"},
	} {
		cols, err := columnsOf(ctx, s.db, c.table)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cols[c.column]; !ok {
			t.Fatalf("%s has no column %s", c.table, c.column)
		}
	}

	// 外键在
	err = s.SaveToken(ctx, Token{AccountID: "nope", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()})
	if err == nil {
		t.Fatal("a token of an account that is not there was accepted")
	}

	// 自增的号接着走
	if err := s.SaveCheckin(ctx, Checkin{AccountID: "acc_01", Day: "2026-11-14",
		Outcome: "claimed", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListCheckins(ctx, "acc_01", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.Day == "2026-11-14" && c.ID <= 1 {
			t.Fatalf("the new check-in has the id %d, want one after the old ones", c.ID)
		}
	}
}

// TestMigrateIsIdempotent says the migration can be done twice, and
// the second time changes nothing.
//
// 迁移做两遍也没关系。
func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("the file is at version %d, want %d", version, schemaVersion)
	}
	if _, err := s.GetAccount(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}
}

// TestTheNameIsWrittenOnItsOwn says the name setter writes the name
// and nothing else.
//
// 改名只写名字，别的列不碰。
func TestTheNameIsWrittenOnItsOwn(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	// 状态那一列一被写就报错，所以改名字的时候它一定没被写。
	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER no_status_write BEFORE UPDATE OF status ON accounts
		BEGIN SELECT RAISE(ABORT, 'the status was written'); END`); err != nil {
		t.Fatal(err)
	}

	if err := s.SetAccountName(ctx, "acc_01", "renamed", time.Now()); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "renamed" {
		t.Fatalf("the name is %q", a.Name)
	}
	if a.Status != StatusActive {
		t.Fatalf("the status is %q", a.Status)
	}

	// 没有的账号是找不到，不是改成功。
	err = s.SetAccountName(ctx, "nope", "x", time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want a not-found", err)
	}
}

// TestTheLastErrorIsWrittenOnItsOwn says the last-error setter writes
// the error and neither the status nor anything else.
//
// 记一条错误只写错误，不改状态。
func TestTheLastErrorIsWrittenOnItsOwn(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER no_status_write BEFORE UPDATE OF status ON accounts
		BEGIN SELECT RAISE(ABORT, 'the status was written'); END`); err != nil {
		t.Fatal(err)
	}

	if err := s.SetAccountLastError(ctx, "acc_01", "the site said no", time.Now()); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAccount(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if a.LastError != "the site said no" {
		t.Fatalf("the last error is %q", a.LastError)
	}
	if a.Status != StatusActive {
		t.Fatalf("the status is %q", a.Status)
	}

	err = s.SetAccountLastError(ctx, "nope", "x", time.Now())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want a not-found", err)
	}
}

// TestThePlanErrorIsKept says the plan's error is written down with
// the reading, so a plan whose title could not be read is not a plan
// without a title.
//
// 套餐名读不出来，要记下来。
func TestThePlanErrorIsKept(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	makeAccount(t, s, "acc_01")

	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01",
		Granted:   100, Consumed: 10, Remaining: 90,
		HasAmounts: true,
		PlanError:  "the plan title could not be read",
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	snaps, err := s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 {
		t.Fatalf("got %d readings", len(snaps))
	}
	if snaps[0].PlanError != "the plan title could not be read" {
		t.Fatalf("the plan error is %q", snaps[0].PlanError)
	}

	// 没有错的读数，读回来也是空的，不是上一条留下的。
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01",
		Granted:   100, Consumed: 20, Remaining: 80,
		HasAmounts: true,
		CreatedAt:  time.Now().Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	snaps, err = s.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].PlanError != "" {
		t.Fatalf("got %+v", snaps)
	}
}

// TestTheFileAtVersionTwoGetsThePlanErrorColumn says a file that was
// made before the plan error column is brought forward by the third
// step.
//
// 版本二的库要补上这一列，行还在。
func TestTheFileAtVersionTwoGetsThePlanErrorColumn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "v2.db")

	// 先做一个新库，再把那一列去掉、把版本写回二，就像上一个版本存下的那样。
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	makeAccount(t, s, "acc_01")
	if err := s.SaveUsageSnapshot(ctx, UsageSnapshot{
		AccountID: "acc_01", Granted: 600, Consumed: 100, Remaining: 500,
		HasAmounts: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE usage_snapshots DROP COLUMN plan_error"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 再打开就该被补上。
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s2.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("the file is at version %d, want %d", version, schemaVersion)
	}
	cols, err := columnsOf(ctx, s2.db, "usage_snapshots")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cols["plan_error"]; !ok {
		t.Fatal("usage_snapshots has no column plan_error")
	}
	// 行还在，读数还是那一条。
	snaps, err := s2.LatestUsageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Remaining != 500 {
		t.Fatalf("got %+v", snaps)
	}
	if snaps[0].PlanError != "" {
		t.Fatalf("the plan error is %q", snaps[0].PlanError)
	}
}

// TestThePasswordGoesInAndComesBack says the encrypted password can be
// written and read back.
func TestThePasswordGoesInAndComesBack(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateAccount(ctx, Account{ID: "acc_1", Name: "a", Status: StatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, "acc_1", "Y2lwaGVy", now); err != nil {
		t.Fatal(err)
	}
	got, err := s.Password(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Y2lwaGVy" {
		t.Fatalf("password = %q", got)
	}
}

// TestAnAccountWithoutAPassword says an account that was not brought
// in by a batch import has no password, and that is not an error.
func TestAnAccountWithoutAPassword(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateAccount(ctx, Account{ID: "acc_1", Name: "a", Status: StatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Password(ctx, "acc_1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("password = %q, want none", got)
	}
}

// TestTheImportRunIsKept says a batch import is written down with what
// came of it.
func TestTheImportRunIsKept(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateImportRun(ctx, ImportRun{ID: "run_1", Source: "cli", StartedAt: now, Total: 2}); err != nil {
		t.Fatal(err)
	}
	err := s.FinishImportRun(ctx, "run_1", now, 1, 1, `[{"email":"a@example.com","ok":true}]`)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListImportRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs", len(runs))
	}
	if runs[0].OK != 1 || runs[0].Failed != 1 || runs[0].Results == "" {
		t.Fatalf("run = %+v", runs[0])
	}
}

// TestAnImportRunCanBeAskedForByItsName says one batch import can be
// taken out on its own.
//
// 页面上要看得见某一次导入走到哪儿了：光有"最近的几次"不够，
// 得能按名字把这一次导入单独取出来。
func TestAnImportRunCanBeAskedForByItsName(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	if err := s.CreateImportRun(ctx, ImportRun{ID: "run_1", Source: "web", StartedAt: now, Total: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishImportRun(ctx, "run_1", now, 1, 1, "[]"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetImportRun(ctx, "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "run_1" || got.Source != "web" || got.OK != 1 || got.Failed != 1 {
		t.Fatalf("run = %+v", got)
	}
}

// TestAnImportRunThatIsNotThere says a name that is not there is an
// error, and not a quiet empty run.
func TestAnImportRunThatIsNotThere(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.GetImportRun(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get import run: %v, want ErrNotFound", err)
	}
}

// TestTheOldFileGetsThePasswordColumn says a file that was made before
// the password column is brought forward: the column is there, the
// rows are still in it, and the accounts that had no password have
// none.
//
// 老库要有 password 那一列，行还要在，没有密码的账号还是 NULL。
func TestTheOldFileGetsThePasswordColumn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	makeAccount(t, s, "acc_01")
	if err := s.SaveToken(ctx, Token{AccountID: "acc_01", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE accounts DROP COLUMN password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP TABLE import_runs"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 再打开就该被补上。
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := s2.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("the file is at version %d, want %d", version, schemaVersion)
	}
	cols, err := columnsOf(ctx, s2.db, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cols["password"]; !ok {
		t.Fatal("accounts has no column password")
	}
	// 行还在
	if _, err := s2.GetAccount(ctx, "acc_01"); err != nil {
		t.Fatal(err)
	}
	// 账本也还在
	tok, err := s2.GetToken(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if tok.Access != "a" {
		t.Fatalf("got %+v", tok)
	}
	// 没有密码的账号，那一格是空的，不是空字符串
	var isNull bool
	if err := s2.db.QueryRowContext(ctx,
		"SELECT password IS NULL FROM accounts WHERE id = ?", "acc_01").Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Fatal("the password of an account that never had one is not NULL")
	}
	got, err := s2.Password(ctx, "acc_01")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("password = %q, want none", got)
	}
	// 导入留痕的表也有了
	if err := s2.CreateImportRun(ctx, ImportRun{ID: "run_1", Source: "cli",
		StartedAt: time.Now(), Total: 1}); err != nil {
		t.Fatal(err)
	}
}
