package store

import (
	"context"
	"sync"
	"testing"
)

// TestTheUpstreamIDIsTheOneInTheRow says the id the store gives is the
// one in the row.
//
// 一条语句也得给出同一个答案：那一行里写着的和给出去的是同一个。
func TestTheUpstreamIDIsTheOneInTheRow(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	id, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("the upstream id is empty")
	}
	var inRow string
	if err := s.db.QueryRowContext(ctx,
		"SELECT upstream_id FROM sessions WHERE session_key=?", "sess-1").Scan(&inRow); err != nil {
		t.Fatal(err)
	}
	if inRow != id {
		t.Errorf("the id in the row = %q, the id given = %q", inRow, id)
	}
}

// TestTheUpstreamIDIsKept says the session keeps the id it was given.
//
// 先到先得：两个请求同时问同一个会话，拿到的是同一个 id —— 上游才认得出
// 这是同一段对话。
func TestTheUpstreamIDIsKept(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	first, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.UpstreamID(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("the ids = %q and %q, want the same one", first, second)
	}
}

// TestTwoCallsAtTheSameTimeGetOneID says two calls at the same time get
// the same id.
//
// 两个请求同时开一段新对话：它们拿到的是同一个 id —— 那是同一段对话，
// 上游才接得上。
func TestTwoCallsAtTheSameTimeGetOneID(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	ids := make([]string, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = s.UpstreamID(ctx, "sess-1")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("the call %d: %v", i, err)
		}
	}
	if ids[0] != ids[1] {
		t.Errorf("the ids = %q and %q, want the same one", ids[0], ids[1])
	}
	var inRow string
	if err := s.db.QueryRowContext(ctx,
		"SELECT upstream_id FROM sessions WHERE session_key=?", "sess-1").Scan(&inRow); err != nil {
		t.Fatal(err)
	}
	if inRow != ids[0] {
		t.Errorf("the id in the row = %q, the ids given = %q and %q", inRow, ids[0], ids[1])
	}
}

// TestTheUpstreamIDIsOneStatement says the upstream id costs one
// statement.
//
// 从前是三条：读一遍、写一遍、再读一遍（最后一遍是为了知道别人有没有先写一
// 个）。现在是一条：写完就把这一行最后的上游 id 拿回来。
func TestTheUpstreamIDIsOneStatement(t *testing.T) {
	ctx := context.Background()
	counter := NewCounter()
	s := openTestOpts(t, WithConnector(counter.Connector()))

	if _, err := s.UpstreamID(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	if got := counter.CountOf("INSERT INTO sessions"); got != 1 {
		t.Errorf("the statements that wrote the session = %d, want 1", got)
	}
	if got := counter.CountOf("SELECT upstream_id FROM sessions"); got != 0 {
		t.Errorf("the statements that read the session = %d, want 0", got)
	}
}

// TestTheSessionStateIsTheRowInOneRead says the session's row comes in
// one read.
//
// 那一行的两列（钉在哪个号上、上游的 id）是同一行：分开问就是两次往返，而
// 每个请求都要问这一行。
func TestTheSessionStateIsTheRowInOneRead(t *testing.T) {
	ctx := context.Background()
	counter := NewCounter()
	s := openTestOpts(t, WithConnector(counter.Connector()))
	makeAccount(t, s, "one")

	if err := s.BindSession(ctx, "sess-1", "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpstreamID(ctx, "sess-1"); err != nil {
		t.Fatal(err)
	}
	counter.Reset()

	state, err := s.SessionState(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Known {
		t.Error("the session was not read at all")
	}
	if state.AccountID != "one" {
		t.Errorf("the account = %q, want one", state.AccountID)
	}
	if state.UpstreamID == "" {
		t.Error("the upstream id is empty")
	}
	if got := counter.CountOf("SELECT account_id, upstream_id FROM sessions"); got != 1 {
		t.Errorf("the statements that read the session row = %d, want 1", got)
	}
}

// TestTheSessionStateOfASessionThatIsNotThere says a session that has no
// row is read, not missing.
//
// 「没读过」跟「读过、那一行是空的」是两件事：分不开的话，轮转会以为每一段
// 对话都没有绑定的号，每一次都重新绑。
func TestTheSessionStateOfASessionThatIsNotThere(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	state, err := s.SessionState(ctx, "sess-nope")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Known {
		t.Error("the session was not read at all")
	}
	if state.AccountID != "" || state.UpstreamID != "" {
		t.Errorf("the state = %+v, want empty", state)
	}
}
