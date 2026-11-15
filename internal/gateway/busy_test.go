package gateway

import (
	"context"
	"testing"
	"time"

	"mmxcode/internal/config"
)

// TestAnAccountTakesOneRequestAtATime says an account takes one
// request at a time.
//
// 两个请求落在一个账号上，额度掉得就快一倍：它们用的是同一份额度。
func TestAnAccountTakesOneRequestAtATime(t *testing.T) {
	b := newBusyAccounts(1)
	if !b.take("acc_1") {
		t.Fatal("the account did not take the request")
	}
	if b.take("acc_1") {
		t.Fatal("the account took a second request")
	}
	b.give("acc_1")
	if !b.take("acc_1") {
		t.Fatal("the account did not take the request again")
	}
}

// TestTheTotalConcurrencySaysHowMany says the total concurrency for a
// number of accounts.
//
// 总并发 = max(floor(可用账号数 / 5), min(可用账号数, 3))
func TestTheTotalConcurrencySaysHowMany(t *testing.T) {
	cfg := config.Default().Concurrency
	for _, c := range []struct{ usable, want int }{
		{1, 1}, {3, 3}, {5, 3}, {12, 3}, {100, 20},
	} {
		if got := totalFor(c.usable, cfg); got != c.want {
			t.Errorf("the total concurrency for %d accounts = %d, want %d",
				c.usable, got, c.want)
		}
	}
}

// TestTheGateLetsTheRequestsIn says the gate holds the requests that
// fit.
//
// 超了的请求排队等：等不到（客户端自己超时了）就 429 带 Retry-After。
func TestTheGateLetsTheRequestsIn(t *testing.T) {
	g := &gate{}
	if !g.take(2) {
		t.Fatal("the first request did not get in")
	}
	if !g.take(2) {
		t.Fatal("the second request did not get in")
	}
	if g.take(2) {
		t.Fatal("a third request got in under a limit of two")
	}
	g.give()
	if !g.take(2) {
		t.Fatal("the request did not get in after one was done")
	}
}

// TestTheGateWaitsForAPlace says a request waits for a free place.
//
// 只有 1 个账号的时候它忙，第二个请求要等它做完，而不是报错。
func TestTheGateWaitsForAPlace(t *testing.T) {
	g := &gate{}
	if !g.take(1) {
		t.Fatal("the first request did not get in")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		g.give()
	}()
	if !g.waitFor(context.Background(), 1, time.Second) {
		t.Fatal("the request did not wait for a place")
	}
	g.give()
}
