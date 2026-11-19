package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestTheRetryAfterIsRead says the upstream's Retry-After is read.
//
// 它有两种写法：一个秒数，一个 HTTP 日期。两个都要认。
func TestTheRetryAfterIsRead(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{"120", 120 * time.Second, true},
		{"0", 0, true},
		{"", 0, false},
		{"soon", 0, false},
		{"-5", 0, false},
		{"Wed, 21 Oct 2026 12:00:30 GMT", 30 * time.Second, true},
		// 已经过去的日期：不是没有，是一个零（现在就可以再来）。
		{"Wed, 21 Oct 2026 11:59:00 GMT", 0, true},
	} {
		got, ok := retryAfter(c.header, now)
		if ok != c.ok || got != c.want {
			t.Errorf("the Retry-After %q = %s (%v), want %s (%v)",
				c.header, got, ok, c.want, c.ok)
		}
	}
}

// TestTheBackoffIsNotTheSameEveryTime says two requests that hit the
// same limit do not back off at the same moment.
//
// 一起退避、一起再问，是惊群：每一遍都正好落在同一个限流窗口的边上。
func TestTheBackoffIsNotTheSameEveryTime(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		seen[backoff(0)] = true
	}
	if len(seen) < 2 {
		t.Errorf("the backoffs = %v: they are all the same", seen)
	}
}

// TestTheBackoffGrowsAndStops says the backoff grows and then stops.
//
// 抖动是加在上面的：形状不能变 —— 半秒、一秒、两秒、四秒，封顶四秒。
func TestTheBackoffGrowsAndStops(t *testing.T) {
	if backoff(0) >= backoff(1) || backoff(1) >= backoff(2) {
		t.Errorf("the backoffs = %s, %s, %s: they do not grow",
			backoff(0), backoff(1), backoff(2))
	}
	if got := backoff(10); got > 4*time.Second {
		t.Errorf("the backoff = %s, want at most 4s", got)
	}
}

// TestTheUpstreamSaysHowLongToWait says the gateway waits as long as the
// upstream says.
//
// 上游说「1 秒后再来」，网关就该 1 秒后再问 —— 不是它自己的那个半秒。
// 限流窗口里敲门，上游的次数就白烧了。
func TestTheUpstreamSaysHowLongToWait(t *testing.T) {
	var hits atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			// 限流：身体空的。里面写「额度」那类词的话，那是「额度用完」
			// （换号的那条路），不是限流。
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	start := time.Now()
	h.gw.ServeHTTP(w, req)
	took := time.Since(start)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if took < time.Second {
		t.Errorf("the request took %s, want at least 1s: the upstream's Retry-After was not heard", took)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("the upstream was asked %d times, want 2", n)
	}
}

// TestTheUpstreamSaysToWaitTooLong says the gateway does not wait for a
// window it cannot reach.
//
// 上游说「一小时后再来」：等是等不到的 —— 客户端等不起，那个号也不该被
// 占着一小时。不等，换一个号。
func TestTheUpstreamSaysToWaitTooLong(t *testing.T) {
	var hits atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	// 请求自己有个时限：网关要是真的去等那一小时，这个时限会让它停下，
	// 测试红，而不是把测试挂死。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	start := time.Now()
	h.gw.ServeHTTP(w, req)
	took := time.Since(start)
	if took > 3*time.Second {
		t.Errorf("the request took %s: it waited for a window of an hour", took)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("the upstream was asked %d times, want 1: the same account was asked again", n)
	}
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("code = %d, want 429", w.Code)
	}
}
