package store

import (
	"context"
	"testing"
	"time"
)

// TestTheAnswerIsKeptUntilSomebodyChangesAKey says the answer to "has
// anybody asked for a key yet" is kept in memory, and is thrown away when
// somebody changes a key.
//
// 这个答案是「要不要鉴权」：它慢了半秒就是半秒的窗口里不带 key 的请求被
// 放行。所以它不能只靠 TTL 过期：谁改了谁就通知。
//
// 「一把都没有」跟「没查过」必须分开：分不开的话缓存永远不命中（每次都
// 当没查过），那这个缓存就白做了。
func TestTheAnswerIsKeptUntilSomebodyChangesAKey(t *testing.T) {
	ctx := context.Background()
	counter := NewCounter()
	s := openTestOpts(t, WithConnector(counter.Connector()))

	// 一把都没有：第一次去查库。
	have, err := s.HasAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if have {
		t.Fatal("there is no key, but the store says there is one")
	}
	if got := counter.CountOf("SELECT COUNT(*) FROM api_keys"); got != 1 {
		t.Fatalf("the statements that counted the keys = %d, want 1", got)
	}

	// 再问五次：不查库了（答案留着）。
	counter.Reset()
	for i := 0; i < 5; i++ {
		have, err := s.HasAPIKeys(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if have {
			t.Fatal("there is still no key, but the store says there is one")
		}
	}
	if got := counter.CountOf("SELECT COUNT(*) FROM api_keys"); got != 0 {
		t.Fatalf("the statements that counted the keys = %d, want 0: the answer is kept", got)
	}

	// 申请了一把 key：那个答案变了，扔掉它 —— 下一个问的人得看见新的。
	if err := s.CreateAPIKey(ctx, APIKey{
		ID: "key_1", Name: "n", KeyHash: "h", Prefix: "p", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	have, err = s.HasAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !have {
		t.Fatal("a key was made, but the store says there is none")
	}

	// 撤销了：答案又变了，扔掉它。
	if err := s.RevokeAPIKey(ctx, "key_1", time.Now()); err != nil {
		t.Fatal(err)
	}
	have, err = s.HasAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if have {
		t.Fatal("the key was taken away, but the store says there is one")
	}
}

// TestTheAnswerIsNotStale says a key made while somebody was asking does
// not get lost.
//
// 一个正在查库的请求会在别人申请了 key 之后，把「一把都没有」这个旧答案
// 写回去 —— 那个答案留在缓存里不动了，门就一直开着。所以查库和写答案要
// 锁在一起（见 HasAPIKeys）。
func TestTheAnswerIsNotStale(t *testing.T) {
	ctx := context.Background()
	counter := NewCounter()
	s := openTestOpts(t, WithConnector(counter.Connector()))

	// 先把「没查过」这个状态坐实：问一次，答案是「没有 key」。
	if _, err := s.HasAPIKeys(ctx); err != nil {
		t.Fatal(err)
	}

	// 有人申请了一把 key。
	if err := s.CreateAPIKey(ctx, APIKey{
		ID: "key_1", Name: "n", KeyHash: "h", Prefix: "p", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// 问出来的答案得是「有 key」。
	have, err := s.HasAPIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !have {
		t.Fatal("a key was made, but the store says there is none")
	}
	if got := counter.CountOf("SELECT COUNT(*) FROM api_keys"); got != 2 {
		t.Fatalf("the statements that counted the keys = %d, want 2: the answer was thrown away", got)
	}
}
