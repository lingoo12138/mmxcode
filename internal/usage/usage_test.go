package usage

import (
	"testing"
	"time"

	"mmxcode/internal/store"
)

func TestAggregate(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	snaps := []store.UsageSnapshot{
		{
			AccountID: "acc_01", HasAmounts: true,
			Granted: 600, Consumed: 120.5, Remaining: 479.5,
			ExpiresAt: now.Add(48 * time.Hour),
		},
		{
			AccountID: "acc_02", HasAmounts: true,
			Granted: 300, Consumed: 0, Remaining: 300,
			ExpiresAt: now.Add(24 * time.Hour),
		},
	}
	s := Aggregate(snaps, now)
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
}

func TestAggregateSkipsAccountsWithoutAmounts(t *testing.T) {
	now := time.Now()
	snaps := []store.UsageSnapshot{
		{AccountID: "acc_01", HasAmounts: true, Granted: 10, Consumed: 1, Remaining: 9},
		{AccountID: "acc_02", HasAmounts: false},
	}
	s := Aggregate(snaps, now)
	if s.TotalGranted != 10 {
		t.Fatalf("granted = %v, want 10", s.TotalGranted)
	}
	if s.AccountsUnavailable != 1 {
		t.Fatalf("unavailable = %d, want 1", s.AccountsUnavailable)
	}
	if s.Accounts[1].Granted != nil {
		t.Fatal("an account without amounts must not carry a number")
	}
}

func TestAggregateEmpty(t *testing.T) {
	s := Aggregate(nil, time.Now())
	if s.TotalGranted != 0 || s.TotalRemaining != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if s.ExpiresAt != nil {
		t.Fatal("no expiry without accounts")
	}
}
