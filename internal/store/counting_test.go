package store

import (
	"context"
	"testing"
	"time"
)

// TestTheCounterCountsTheStatements says the counter counts the
// statements that went to the database.
//
// 量具本身要有牙：它数出来的得是真的语句，而且一条不多一条不少。
func TestTheCounterCountsTheStatements(t *testing.T) {
	ctx := context.Background()
	counter := NewCounter()
	s := openTestOpts(t, WithConnector(counter.Connector()))
	makeAccount(t, s, "one")

	if err := s.SaveCheckin(ctx, Checkin{
		AccountID: "one", Day: "2026-11-19", Outcome: "claimed", Credit: 25,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if got := counter.CountOf("INSERT INTO checkins"); got != 1 {
		t.Errorf("the statements that inserted a check-in = %d, want 1", got)
	}
	if got := counter.Count(); got == 0 {
		t.Error("nothing was counted at all")
	}
}

// TestTheCounterCountsEveryConnection says the statements of every
// connection are counted.
//
// 丢不得的那两笔走的是另一条连接：那条上的语句也得算进去，否则量出来的
// 条数是少的。
func TestTheCounterCountsEveryConnection(t *testing.T) {
	ctx := context.Background()
	counter := NewCounter()
	// 要「normal」才有那两条连接：常用的那条不 fsync，丢不得的那条才另开
	// 一条（synchronous=full）。默认的时候 SQLite 自己就是 full，两条是同
	// 一条 —— 那样这条测试就只是在数同一条连接上的语句，没有测到「另一
	// 条」这件事。
	s := openTestOpts(t, WithConnector(counter.Connector()), WithSynchronous("normal"))
	makeAccount(t, s, "one")

	// 这一笔走的是那条 fsync 的连接。
	if err := s.SaveToken(ctx, Token{
		AccountID: "one", Access: "a", Refresh: "r",
		ExpiresAt: time.Now().Add(time.Hour), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := counter.CountOf("INSERT INTO tokens"); got != 1 {
		t.Errorf("the statements that wrote a token = %d, want 1", got)
	}
}
