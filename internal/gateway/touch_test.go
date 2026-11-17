package gateway

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/store"
)

// TestTheUsedSessionsAreWritten says a session that was used is written
// back into the database.
//
// 「用过」是活跃会话唯一的凭据：网关不问上游，它只知道这段对话被用过
// （规格 §6.3）。不写回去，池子就不会留着它绑着的号。
func TestTheUsedSessionsAreWritten(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "a-conversation", "acc_01"); err != nil {
		t.Fatal(err)
	}
	g := &Server{Store: s, Cfg: config.Default(), touches: newSessionTouches()}

	// 这个会话两小时前用过，之后没人碰它。
	s.Clock = func() time.Time { return now.Add(-2 * time.Hour) }
	g.touches.add("a-conversation")
	g.flushTouches(ctx)
	s.Clock = time.Now

	n, err := s.CountRecentSessions(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recent sessions = %d, want 0", n)
	}

	// 刚用过：它是活跃的。
	g.touches.add("a-conversation")
	g.flushTouches(ctx)
	n, err = s.CountRecentSessions(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recent sessions = %d, want 1", n)
	}
}

// TestTheUsedSessionsAreWrittenOnlyOnce says a session that was used
// twice is written once.
//
// 写的是「用过没有」，不是「用了几次」。
func TestTheUsedSessionsAreWrittenOnlyOnce(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAccount(ctx, store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "a-conversation", "acc_01"); err != nil {
		t.Fatal(err)
	}
	g := &Server{Store: s, Cfg: config.Default(), touches: newSessionTouches()}

	g.touches.add("a-conversation")
	g.touches.add("a-conversation")
	keys := g.touches.take()
	if len(keys) != 1 {
		t.Fatalf("keys = %v, want one", keys)
	}
	// 拿了就是归网关了：下一批不该再有它。
	if again := g.touches.take(); len(again) != 0 {
		t.Fatalf("keys = %v, want none", again)
	}
}

// TestTheAccountAConversationIsBoundToComesFirst says the account a live
// conversation is bound to is the first one the pool takes.
//
// 会话钉在号上，钉住的号不在池里就会被改绑，缓存命中就没了（规格不变
// 量 3）。
func TestTheAccountAConversationIsBoundToComesFirst(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_busy", "acc_quiet"} {
		if err := s.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 绑着的那号是被用过的：它有读数，是热的。
	if err := s.SaveUsageSnapshot(ctx, store.UsageSnapshot{
		AccountID: "acc_busy", Granted: 400, Remaining: 400, HasAmounts: true, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.BindSession(ctx, "a-conversation", "acc_busy"); err != nil {
		t.Fatal(err)
	}

	src := poolSource{s: s, window: time.Hour}
	cands, err := src.Candidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	if cands[0].Account.ID != "acc_busy" {
		t.Fatalf("candidates = %v, want acc_busy first", cands)
	}
}

// TestWithoutAWindowNoConversationHoldsAPlace says a gateway with no
// session window counts no conversation as active.
//
// 窗口是「多久算活跃」的那把尺子：没有它就没有「活跃」，一个号也不因
// 为对话而排在前面。
func TestWithoutAWindowNoConversationHoldsAPlace(t *testing.T) {
	s := openGatewayStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_busy", "acc_quiet"} {
		if err := s.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveUsageSnapshot(ctx, store.UsageSnapshot{
		AccountID: "acc_busy", Granted: 400, Remaining: 400, HasAmounts: true, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// 这个会话两小时前绑的：没有窗口的时候它不算活跃。
	s.Clock = func() time.Time { return now.Add(-2 * time.Hour) }
	if err := s.BindSession(ctx, "a-conversation", "acc_busy"); err != nil {
		t.Fatal(err)
	}
	s.Clock = time.Now

	src := poolSource{s: s}
	cands, err := src.Candidates(ctx, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2", len(cands))
	}
	if cands[0].Account.ID != "acc_quiet" {
		t.Fatalf("candidates = %v, want acc_quiet first", cands)
	}
}
