package store

import (
	"context"
	"testing"
	"time"
)

// saveReading puts a reading of an account's credits in the store.
//
// 读到的时刻是可以说了算的：那个时刻就是「这个号有多冷」的凭据。
func saveReading(t *testing.T, s *Store, accountID string, remaining float64, at time.Time) {
	t.Helper()
	err := s.SaveUsageSnapshot(context.Background(), UsageSnapshot{
		AccountID:  accountID,
		Granted:    400,
		Consumed:   400 - remaining,
		Remaining:  remaining,
		HasAmounts: true,
		CreatedAt:  at,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTheCandidatesAreTheColdestFirst says the pool is filled with the
// accounts that were used least, not the ones that were used most.
//
// 冷号先进池：被用熟的号留给会话（规格 §6.5）。从来没读过的号排在最
// 前 —— 它还没有数，读一次就知道它有没有额度。
func TestTheCandidatesAreTheColdestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "warm")
	makeAccount(t, s, "cold")
	makeAccount(t, s, "never")
	now := time.Now()
	saveReading(t, s, "warm", 10, now.Add(-time.Hour))
	saveReading(t, s, "cold", 400, now.Add(-24*time.Hour))

	cands, err := s.PoolCandidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cands {
		ids = append(ids, c.Account.ID)
	}
	want := []string{"never", "cold", "warm"}
	if len(ids) != len(want) {
		t.Fatalf("candidates = %v, want %v", ids, want)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Fatalf("candidates = %v, want %v", ids, want)
		}
	}
}

// TestACandidateThatWasNeverReadSaysNothingAboutItsCredits says the
// credits of an account that was never read are not zero.
//
// 它是「没读到」，不是「没有」：写成 0 就是撒谎，撒谎的数会被当成真的
// 数去挑号（规格不变量 2）。
func TestACandidateThatWasNeverReadSaysNothingAboutItsCredits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")

	cands, err := s.PoolCandidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	if cands[0].Known {
		t.Fatal("the credits of an account that was never read cannot be known")
	}
	if cands[0].Remaining != 0 {
		t.Fatalf("remaining = %v, want 0", cands[0].Remaining)
	}
}

// TestACandidateThatWasReadCarriesItsCredits says a candidate that was
// read carries the credits it was read with.
func TestACandidateThatWasReadCarriesItsCredits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	saveReading(t, s, "a", 123, time.Now())

	cands, err := s.PoolCandidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	if !cands[0].Known {
		t.Fatal("the credits of an account that was read are known")
	}
	if cands[0].Remaining != 123 {
		t.Fatalf("remaining = %v, want 123", cands[0].Remaining)
	}
}

// TestTheAccountsInThePoolAreNotCandidates says the accounts that are
// already in the pool are not offered again.
func TestTheAccountsInThePoolAreNotCandidates(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	makeAccount(t, s, "b")

	cands, err := s.PoolCandidates(ctx, 10, map[string]bool{"a": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Account.ID != "b" {
		t.Fatalf("candidates = %v, want just b", cands)
	}
}

// TestAnAccountThatIsSpentIsNotACandidate says an account whose window
// is spent is not offered to the pool.
//
// 它的窗口什么时候回来，标记说了算：标记还在将来，它就接不了活。
func TestAnAccountThatIsSpentIsNotACandidate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	makeAccount(t, s, "b")
	if err := s.SetExhausted(ctx, "a", "", time.Now().Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}

	cands, err := s.PoolCandidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.Account.ID == "a" {
			t.Fatal("a spent account is not a candidate")
		}
	}
}

// TestAnAccountThatIsSignedOutIsNotACandidate says an account that is
// not signed in is not offered to the pool.
func TestAnAccountThatIsSignedOutIsNotACandidate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	if err := s.UpdateAccountStatus(ctx, "a", StatusSignedOut, "", time.Now()); err != nil {
		t.Fatal(err)
	}

	cands, err := s.PoolCandidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates = %v, want none", cands)
	}
}

// TestTheUsableAccountsAreTheOnesThatCanAnswer says the accounts that
// are signed in and not spent are the ones that can answer.
func TestTheUsableAccountsAreTheOnesThatCanAnswer(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	makeAccount(t, s, "b")
	makeAccount(t, s, "c")
	if err := s.SetExhausted(ctx, "b", "", time.Now().Add(time.Hour), "quota"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAccountStatus(ctx, "c", StatusSignedOut, "", time.Now()); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountUsable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("usable = %d, want 1", n)
	}
}

// TestTheRecentSessionsAreCounted says the sessions that were touched
// lately are counted, and the old ones are not.
//
// 池要装得下活跃的会话：会话钉在号上，钉住的号不在池里会被改绑，缓存
// 命中就没了（规格 §6.3）。
func TestTheRecentSessionsAreCounted(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	if err := s.BindSession(ctx, "recent", "a"); err != nil {
		t.Fatal(err)
	}

	n, err := s.CountRecentSessions(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recent sessions = %d, want 1", n)
	}
	n, err = s.CountRecentSessions(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recent sessions = %d, want 0", n)
	}
}
