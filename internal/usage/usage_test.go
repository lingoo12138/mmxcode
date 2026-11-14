package usage

import (
	"testing"
	"time"

	"mmxcode/internal/store"
)

// accountOf gives the entry of one account.
//
// The accounts are sorted by id, but a test that counts on the order
// breaks when an account is added: the account is asked for by name.
func accountOf(t *testing.T, s Summary, id string) AccountUsage {
	t.Helper()
	for _, a := range s.Accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no entry for %s in %+v", id, s.Accounts)
	return AccountUsage{}
}

func TestAggregate(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 600, Consumed: 120.5, Remaining: 479.5,
			ExpiresAt: now.Add(48 * time.Hour),
			CreatedAt: now.Add(-time.Minute),
		},
		{
			AccountID: "acc_02", HasAmounts: true,
			Granted: 300, Consumed: 0, Remaining: 300,
			ExpiresAt: now.Add(24 * time.Hour),
			CreatedAt: now.Add(-time.Minute),
		},
	}
	s := Aggregate(snaps, nil, now)
	if s.TotalGranted != 900 {
		t.Fatalf("granted = %v, want 900", s.TotalGranted)
	}
	if s.TotalConsumed != 120.5 {
		t.Fatalf("consumed = %v, want 120.5", s.TotalConsumed)
	}
	if s.TotalRemaining != 779.5 {
		t.Fatalf("remaining = %v, want 779.5", s.TotalRemaining)
	}
	if s.ExpiresAt == nil || !s.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("expires_at = %v, want the soonest", s.ExpiresAt)
	}
	if len(s.Accounts) != 2 {
		t.Fatalf("accounts = %d", len(s.Accounts))
	}
	a1 := accountOf(t, s, "acc_01")
	if a1.Granted == nil || *a1.Granted != 600 {
		t.Fatalf("acc_01 = %+v", a1)
	}
	if a1.State != StateRead {
		t.Fatalf("acc_01 state = %q, want %q", a1.State, StateRead)
	}
	a2 := accountOf(t, s, "acc_02")
	if a2.Remaining == nil || *a2.Remaining != 300 {
		t.Fatalf("acc_02 = %+v", a2)
	}
}

func TestAggregateSkipsAccountsWithoutAmounts(t *testing.T) {
	now := time.Now()
	snaps := []store.UsageSnapshot{
		{AccountID: "acc_01", HasAmounts: true, Granted: 10, Consumed: 1, Remaining: 9},
		{AccountID: "acc_02", HasAmounts: false},
	}
	s := Aggregate(snaps, nil, now)
	if s.TotalGranted != 10 {
		t.Fatalf("granted = %v, want 10", s.TotalGranted)
	}
	if s.AccountsUnavailable != 1 {
		t.Fatalf("unavailable = %d, want 1", s.AccountsUnavailable)
	}
	a := accountOf(t, s, "acc_02")
	if a.Granted != nil {
		t.Fatal("an account without amounts must not carry a number")
	}
	if a.State != StateUnavailable {
		t.Fatalf("state = %q, want %q", a.State, StateUnavailable)
	}
}

func TestAggregateEmpty(t *testing.T) {
	s := Aggregate(nil, nil, time.Now())
	if s.TotalGranted != 0 || s.TotalRemaining != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if s.ExpiresAt != nil {
		t.Fatal("no expiry without accounts")
	}
}

// TestAggregateCountsAnAccountOnce says two readings of one account are
// one account.
//
// 同一个账号的两行读数都被加起来，积分就翻倍：一个 100 积分的账号
// 会被报成 300。
func TestAggregateCountsAnAccountOnce(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			ID: 1, AccountID: "acc_01", HasAmounts: true,
			Granted: 100, Consumed: 0, Remaining: 100,
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		},
		{
			// 同一毫秒里的第二次刷新
			ID: 2, AccountID: "acc_01", HasAmounts: true,
			Granted: 100, Consumed: 0, Remaining: 100,
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		},
	}
	s := Aggregate(snaps, nil, now)
	if s.TotalGranted != 100 {
		t.Fatalf("granted = %v, want 100: one account is one account", s.TotalGranted)
	}
	if s.TotalRemaining != 100 {
		t.Fatalf("remaining = %v, want 100", s.TotalRemaining)
	}
	if len(s.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(s.Accounts))
	}
}

// TestAggregateKeepsTheNewestReading says the newer reading of an
// account is the one that counts.
func TestAggregateKeepsTheNewestReading(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			ID: 1, AccountID: "acc_01", HasAmounts: true,
			Granted: 100, Consumed: 0, Remaining: 100,
			ExpiresAt: now.Add(time.Hour), CreatedAt: now.Add(-time.Minute),
		},
		{
			ID: 2, AccountID: "acc_01", HasAmounts: true,
			Granted: 300, Consumed: 0, Remaining: 300,
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		},
	}
	s := Aggregate(snaps, nil, now)
	if s.TotalGranted != 300 {
		t.Fatalf("granted = %v, want 300: the newest reading counts", s.TotalGranted)
	}
	if len(s.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(s.Accounts))
	}
}

// TestAggregateTellsExpiredFromUnread says an account whose credits ran
// out and an account whose credits were never read are not the same.
//
// 两个都说成“读不到”，总数就短了一截，却没有人知道为什么。
func TestAggregateTellsExpiredFromUnread(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 100, Consumed: 100, Remaining: 0,
			ExpiresAt: now.Add(-time.Hour), CreatedAt: now,
		},
		{
			AccountID: "acc_02", HasAmounts: false, CreatedAt: now,
		},
	}
	s := Aggregate(snaps, nil, now)
	if s.TotalGranted != 0 {
		t.Fatalf("granted = %v, want 0: expired credits are not counted", s.TotalGranted)
	}
	if s.AccountsExpired != 1 {
		t.Fatalf("expired = %d, want 1", s.AccountsExpired)
	}
	if s.AccountsUnavailable != 1 {
		t.Fatalf("unavailable = %d, want 1", s.AccountsUnavailable)
	}
	expired := accountOf(t, s, "acc_01")
	if expired.State != StateExpired {
		t.Fatalf("acc_01 state = %q, want %q", expired.State, StateExpired)
	}
	if expired.ExpiresAt == nil {
		t.Fatal("an expired reading has to say when it ran out")
	}
	unread := accountOf(t, s, "acc_02")
	if unread.State != StateUnavailable {
		t.Fatalf("acc_02 state = %q, want %q", unread.State, StateUnavailable)
	}
	if unread.Granted != nil {
		t.Fatal("a reading that never happened has no numbers")
	}
}

// TestAggregateTellsZeroFromUnread says an account with nothing left is
// not an account that was never read.
//
// 真的 0 和读不到不是一回事：读不到的账号不计入总数，真的 0 计入。
func TestAggregateTellsZeroFromUnread(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 0, Consumed: 0, Remaining: 0, CreatedAt: now,
		},
	}
	s := Aggregate(snaps, nil, now)
	if s.AccountsUnavailable != 0 {
		t.Fatalf("unavailable = %d, want 0: a zero is not an unknown", s.AccountsUnavailable)
	}
	if s.AccountsExpired != 0 {
		t.Fatalf("expired = %d, want 0", s.AccountsExpired)
	}
	a := accountOf(t, s, "acc_01")
	if a.State != StateRead {
		t.Fatalf("state = %q, want %q", a.State, StateRead)
	}
	if a.Remaining == nil || *a.Remaining != 0 {
		t.Fatalf("acc_01 = %+v, want a zero that is there", a)
	}
}

// TestAggregateUpdatedAtIsTheReadingTime says the summary says when
// the numbers were read, not when the summary was made.
//
// 汇总时间当成读数时间，陈旧的数字看起来就是新的。
func TestAggregateUpdatedAtIsTheReadingTime(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	read := now.Add(-2 * time.Minute)
	s := Aggregate([]store.UsageSnapshot{
		{AccountID: "acc_01", HasAmounts: true, Granted: 10, Remaining: 10, CreatedAt: read},
	}, nil, now)
	if !s.UpdatedAt.Equal(read) {
		t.Fatalf("updated_at = %v, want the reading time %v", s.UpdatedAt, read)
	}
	// 没有读数时退回汇总时间
	empty := Aggregate(nil, nil, now)
	if !empty.UpdatedAt.Equal(now) {
		t.Fatalf("updated_at = %v, want %v", empty.UpdatedAt, now)
	}
}

// TestAnAccountThatWasNeverReadIsInTheSummary says an account whose
// credits were never read is in the summary.
//
// 一个账号刚导进来、还没到下一次读数的时候，它不是「没有积分」——它是
// 没读过。汇总里少了它，人会以为它没有积分；页面上少了它，人会以为
// 它不在库里。
func TestAnAccountThatWasNeverReadIsInTheSummary(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 600, Consumed: 120.5, Remaining: 479.5,
			ExpiresAt: now.Add(48 * time.Hour),
			CreatedAt: now.Add(-time.Minute),
		},
	}
	accounts := []store.Account{
		{ID: "acc_01", Email: "a@example.com"},
		{ID: "acc_02", Email: "b@example.com"},
	}
	s := Aggregate(snaps, accounts, now)
	got := accountOf(t, s, "acc_02")
	if got.State != StateUnread {
		t.Fatalf("state = %q, want %q", got.State, StateUnread)
	}
	if got.Email != "b@example.com" {
		t.Fatalf("email = %q, want the account's address", got.Email)
	}
	if s.AccountsUnread != 1 {
		t.Fatalf("unread = %d, want 1", s.AccountsUnread)
	}
	// 没读过的账号不算进总数：它不是零，是不知道。
	if s.TotalGranted != 600 {
		t.Fatalf("granted = %v, want 600", s.TotalGranted)
	}
	if s.TotalRemaining != 479.5 {
		t.Fatalf("remaining = %v, want 479.5", s.TotalRemaining)
	}
}
