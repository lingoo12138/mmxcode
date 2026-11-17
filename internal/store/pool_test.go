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

// makeAccountAt puts an account in the store with a said creation time.
//
// 导入的时刻是「这个号有多新」的凭据。
func makeAccountAt(t *testing.T, s *Store, id string, createdAt time.Time) {
	t.Helper()
	err := s.CreateAccount(context.Background(), Account{
		ID: id, Name: id, Status: StatusActive, CreatedAt: createdAt, UpdatedAt: createdAt,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// candidateIDs gives the ids of the candidates, in order.
func candidateIDs(t *testing.T, s *Store, limit int, exclude map[string]bool, since time.Time) []string {
	t.Helper()
	cands, err := s.PoolCandidates(context.Background(), limit, exclude, since)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.Account.ID)
	}
	return ids
}

// TestTheCandidatesAreTheColdestFirst says the pool is filled with the
// accounts that were used least, not the ones that were used most.
//
// 冷号先进池：被用熟的号留给会话（规格 §6.5）。从来没读过的号排在最
// 前 —— 它还没有数，读一次就知道它有没有额度。
func TestTheCandidatesAreTheColdestFirst(t *testing.T) {
	s := openTest(t)
	makeAccount(t, s, "warm")
	makeAccount(t, s, "cold")
	makeAccount(t, s, "never")
	now := time.Now()
	saveReading(t, s, "warm", 10, now.Add(-time.Hour))
	saveReading(t, s, "cold", 400, now.Add(-24*time.Hour))

	ids := candidateIDs(t, s, 10, nil, now.Add(-time.Hour))
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

// TestANewlyImportedAccountComesBeforeAnOldOne says the accounts that
// were never read are taken newest first.
//
// 新导入的号几乎肯定没被用过：用过就会被读，读过就有数。导入很久还没
// 数的号多半是被用过的，它的额度早就不知道剩多少了（规格 §6.5）。
func TestANewlyImportedAccountComesBeforeAnOldOne(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	makeAccountAt(t, s, "old", now.Add(-30*24*time.Hour))
	makeAccountAt(t, s, "new", now)

	ids := candidateIDs(t, s, 10, nil, now.Add(-time.Hour))
	if len(ids) != 2 || ids[0] != "new" || ids[1] != "old" {
		t.Fatalf("candidates = %v, want [new old]", ids)
	}
}

// TestTheAccountsThatConversationsAreBoundToComeFirst says the account
// a live conversation is bound to is the first one into the pool.
//
// 会话钉在号上，钉住的号不在池里就会被改绑，缓存命中就没了（规格不变
// 量 3）。所以池子先装它们。
func TestTheAccountsThatConversationsAreBoundToComeFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "busy")
	makeAccount(t, s, "quiet")
	// 绑着的那号是被用过的：它有读数，是热的。
	saveReading(t, s, "busy", 400, now.Add(-time.Minute))
	if err := s.BindSession(ctx, "a-conversation", "busy"); err != nil {
		t.Fatal(err)
	}

	ids := candidateIDs(t, s, 10, nil, now.Add(-time.Hour))
	if len(ids) != 2 || ids[0] != "busy" {
		t.Fatalf("candidates = %v, want busy first", ids)
	}
}

// TestAConversationThatWasNotUsedLatelyDoesNotComeFirst says a
// conversation that was not used lately does not hold a place in the
// pool.
//
// 「用过」是网关写进去的（TouchSessions）。写不进去的对话不算活跃。
func TestAConversationThatWasNotUsedLatelyDoesNotComeFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "busy")
	makeAccount(t, s, "quiet")
	saveReading(t, s, "busy", 400, now.Add(-time.Minute))
	if err := s.BindSession(ctx, "a-conversation", "busy"); err != nil {
		t.Fatal(err)
	}
	// 把时钟拨回去：这个对话两小时前用过，之后没人碰它。
	s.Clock = func() time.Time { return now.Add(-2 * time.Hour) }
	if err := s.TouchSessions(ctx, []string{"a-conversation"}); err != nil {
		t.Fatal(err)
	}
	s.Clock = time.Now

	ids := candidateIDs(t, s, 10, nil, now.Add(-time.Hour))
	if len(ids) != 2 || ids[0] != "quiet" {
		t.Fatalf("candidates = %v, want quiet first", ids)
	}
}

// TestTheLimitIsALimit says the caller gets at most what it asked for.
//
// 多取的那些行是为了把被排除的补上，但补齐之后要截断 —— 调用方说要多
// 少，就是多少。
func TestTheLimitIsALimit(t *testing.T) {
	s := openTest(t)
	now := time.Now()
	makeAccount(t, s, "a")
	makeAccount(t, s, "b")
	makeAccount(t, s, "c")

	ids := candidateIDs(t, s, 1, nil, now.Add(-time.Hour))
	if len(ids) != 1 {
		t.Fatalf("candidates = %v, want 1", ids)
	}
	// 被排除的那个正好排在前面：多取的行补上了它，截断之后还是 limit 个。
	ids = candidateIDs(t, s, 1, map[string]bool{"a": true}, now.Add(-time.Hour))
	if len(ids) != 1 {
		t.Fatalf("candidates = %v, want 1", ids)
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

	cands, err := s.PoolCandidates(ctx, 10, nil, time.Now().Add(-time.Hour))
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
	if !cands[0].LastReadAt.IsZero() {
		t.Fatalf("last read at = %v, want the zero time", cands[0].LastReadAt)
	}
}

// TestACandidateThatWasReadCarriesItsCredits says a candidate that was
// read carries the credits it was read with.
func TestACandidateThatWasReadCarriesItsCredits(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	at := time.Now().Add(-time.Minute)
	saveReading(t, s, "a", 123, at)

	cands, err := s.PoolCandidates(ctx, 10, nil, time.Now().Add(-time.Hour))
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
	if cands[0].LastReadAt.UnixMilli() != at.UnixMilli() {
		t.Fatalf("last read at = %v, want %v", cands[0].LastReadAt, at)
	}
}

// TestTheAccountsInThePoolAreNotCandidates says the accounts that are
// already in the pool are not offered again.
func TestTheAccountsInThePoolAreNotCandidates(t *testing.T) {
	s := openTest(t)
	makeAccount(t, s, "a")
	makeAccount(t, s, "b")

	ids := candidateIDs(t, s, 10, map[string]bool{"a": true}, time.Now().Add(-time.Hour))
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("candidates = %v, want just b", ids)
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

	ids := candidateIDs(t, s, 10, nil, time.Now().Add(-time.Hour))
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("candidates = %v, want just b", ids)
	}
}

// TestAnAccountWhoseMarkHasExpiredIsACandidate says an account whose
// window is back is offered again.
//
// 标记的时刻过去了，窗口就回来了，那个号又能接活了（规格 §4.5）。
func TestAnAccountWhoseMarkHasExpiredIsACandidate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	makeAccount(t, s, "a")
	if err := s.SetExhausted(ctx, "a", "", time.Now().Add(-time.Minute), "quota"); err != nil {
		t.Fatal(err)
	}

	ids := candidateIDs(t, s, 10, nil, time.Now().Add(-time.Hour))
	if len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("candidates = %v, want just a", ids)
	}
	n, err := s.CountUsable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("usable = %d, want 1", n)
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

	ids := candidateIDs(t, s, 10, nil, time.Now().Add(-time.Hour))
	if len(ids) != 0 {
		t.Fatalf("candidates = %v, want none", ids)
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

// TestTouchingASessionMakesItActive says a session that was used is
// counted as active.
//
// 「用过」只有网关知道：它把用过的会话写回去（规格 §6.3）。写回去的
// 那一批，是一批一次写的。
func TestTouchingASessionMakesItActive(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	makeAccount(t, s, "a")
	if err := s.BindSession(ctx, "old", "a"); err != nil {
		t.Fatal(err)
	}
	// 这个会话两小时前绑的，之后没人碰它。
	s.Clock = func() time.Time { return now.Add(-2 * time.Hour) }
	if err := s.TouchSessions(ctx, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	s.Clock = time.Now

	n, err := s.CountRecentSessions(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recent sessions = %d, want 0", n)
	}
	if err := s.TouchSessions(ctx, []string{"old"}); err != nil {
		t.Fatal(err)
	}
	n, err = s.CountRecentSessions(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recent sessions = %d, want 1", n)
	}
}

// TestTouchingNoSessionIsNothing says touching nothing does not touch
// the database.
//
// 空的批次不该发一条语句出去。
func TestTouchingNoSessionIsNothing(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.TouchSessions(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSessions(ctx, []string{""}); err != nil {
		t.Fatal(err)
	}
}
