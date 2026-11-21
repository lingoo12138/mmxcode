package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lingoo12138/mmxcode/internal/config"
	"github.com/lingoo12138/mmxcode/internal/store"
)

// bigBody is a request body that is larger than the small lane takes.
//
// 它是合法的 JSON：网关要读它（模型名、会话），读不出来就是另一种错。
func bigBody(n int) string {
	return `{"model":"m","pad":"` + strings.Repeat("x", n) + `"}`
}

// addAccounts puts more signed-in accounts in the store.
//
// 网关承诺的并发是从「能用的账号有几个」算出来的：一个号就一个并发。
func addAccounts(t *testing.T, h *harness, ids ...string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	for _, id := range ids {
		if err := h.store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.store.SaveToken(ctx, store.Token{
			AccountID: id, Access: "tok" + id, Refresh: "ref",
			ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTwoBigBodiesDoNotRunAtTheSameTime says a big request body waits for
// the one before it.
//
// 大身体一次只让一个进来：两个一起来那是一百多兆（规格 §5）。
func TestTwoBigBodiesDoNotRunAtTheSameTime(t *testing.T) {
	inTheAir := make(chan struct{}, 2)
	release := make(chan struct{})
	// 放的活儿一定得放：测试要是死在半路上，那个还堵在上游的请求也得
	// 走 —— 不然关假服务器的时候会等它等到天荒地老。
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	defer open()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		inTheAir <- struct{}{}
		<-release
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 三个号才有三个并发位：不然第二个大请求是被并发闸门挡住的，测的
	// 就不是大道了。
	addAccounts(t, h, "acc_02", "acc_03")
	h.gw.concTTL = 0
	big := bigBody(5 << 20)

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(big))
		h.gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-inTheAir:
	case <-time.After(10 * time.Second):
		t.Fatal("the first big request never reached the upstream")
	}

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(big))
		h.gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-inTheAir:
		t.Fatal("the second big request reached the upstream while the first was in the air")
	case <-time.After(300 * time.Millisecond):
	}

	// 第一个走完了，第二个就该进去了。
	open()
	select {
	case <-inTheAir:
	case <-time.After(10 * time.Second):
		t.Fatal("the second big request never reached the upstream")
	}
}

// TestASmallBodyIsNotHeldBackByABigOne says a small request goes while a
// big one is in the air.
//
// 小的走自己的道：它不占大道的位，也不被大道的那个位挡着（规格 §4）。
func TestASmallBodyIsNotHeldBackByABigOne(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// 大的那个在上游那里待着，小的直接答。
		if r.ContentLength > 4<<20 {
			<-release
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 三个号才有两个并发位。
	addAccounts(t, h, "acc_02", "acc_03")
	h.gw.concTTL = 0

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bigBody(5<<20)))
		h.gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	defer close(release)
	// 那个大请求到上游了才算数。
	deadline := time.Now().Add(10 * time.Second)
	for h.hits == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	h.mu.Lock()
	hits := h.hits
	h.mu.Unlock()
	if hits == 0 {
		t.Fatal("the big request never reached the upstream")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// TestABigBodyWaitingForItsLaneDoesNotHoldAConcurrencyPlace says a request
// that waits for the big lane does not take a place of the concurrency
// gate.
//
// 道的门在并发闸门之前：不然 20 个大请求会把 20 个位全占住（它们在等大道
// 那个位），小请求连闸门都进不去 —— 那 19 个小请求就没了（规格 §4）。
func TestABigBodyWaitingForItsLaneDoesNotHoldAConcurrencyPlace(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// 大的那个在上游那里待着（道的门要是没挡住它，它就到这儿来了）。
		if r.ContentLength > 4<<20 {
			<-release
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	h.gw.Cfg.Concurrency.Wait = config.Duration(500 * time.Millisecond)
	defer close(release)
	// 大道的那个位被人占着。
	if !h.gw.bigGate.take(1) {
		t.Fatal("the big lane did not give out its place")
	}
	defer h.gw.bigGate.give()

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bigBody(5<<20)))
		h.gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.Sleep(100 * time.Millisecond)

	// 小请求照常走：并发那一个位还空着。
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// TestABigBodyIsRefusedWhenTheBigLaneIsBusy says a big body that cannot
// get its place gets a 429 that says what is busy.
func TestABigBodyIsRefusedWhenTheBigLaneIsBusy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	h.gw.Cfg.Concurrency.Wait = config.Duration(100 * time.Millisecond)
	if !h.gw.bigGate.take(1) {
		t.Fatal("the big lane did not give out its place")
	}
	defer h.gw.bigGate.give()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(bigBody(5<<20)))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "another big request body is in the air") {
		t.Fatalf("body = %s, want it to say the big lane is busy", w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("the 429 has no Retry-After")
	}
}

// TestABodyWithoutAContentLengthGoesToTheBigLane says a chunked request
// counts as a big one.
//
// 客户端没说多大，那就当它大：宁可慢一点，不能让内存被撑破（规格 §3）。
func TestABodyWithoutAContentLengthGoesToTheBigLane(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	h.gw.Cfg.Concurrency.Wait = config.Duration(100 * time.Millisecond)
	if !h.gw.bigGate.take(1) {
		t.Fatal("the big lane did not give out its place")
	}
	defer h.gw.bigGate.give()

	body := io.NopCloser(strings.NewReader(`{"model":"m"}`))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	if req.ContentLength >= 0 {
		t.Fatalf("the request says it is %d bytes, want it to say nothing", req.ContentLength)
	}
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "another big request body is in the air") {
		t.Fatalf("body = %s, want it to say the big lane is busy", w.Body.String())
	}
}

// TestTheSmallBodiesAreRefusedWhenThePoolIsFull says a small body that
// does not fit in the pool gets a 429 that says what is busy.
func TestTheSmallBodiesAreRefusedWhenThePoolIsFull(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	h.gw.Cfg.Concurrency.Wait = config.Duration(100 * time.Millisecond)
	h.gw.Cfg.Concurrency.BodyBudgetMB = 1
	// 池子满了。
	if !h.gw.bodies.take(1<<20, 1<<20) {
		t.Fatal("the pool did not take the bytes")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "too many request bodies are in the air") {
		t.Fatalf("body = %s, want it to say the bodies are in the air", w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("the 429 has no Retry-After")
	}
}

// TestTheOpenAIBodyCountsTwice says an OpenAI body takes twice the bytes.
//
// OpenAI 和 Codex 的身体要转成 Anthropic 的形状，那是第二份 —— 转换的那一
// 刻两份都在（规格 §6）。
func TestTheOpenAIBodyCountsTwice(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"pad":"` +
		strings.Repeat("x", 300*1024) + `"}`
	if got := heldBytes(t, "/v1/chat/completions", body); got != int64(len(body))*2 {
		t.Fatalf("the pool holds %d bytes, want %d", got, int64(len(body))*2)
	}
}

// TestTheAnthropicBodyCountsOnce says an Anthropic body takes its own
// size: it is sent on as it is, there is no second copy.
func TestTheAnthropicBodyCountsOnce(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"pad":"` +
		strings.Repeat("x", 300*1024) + `"}`
	if got := heldBytes(t, "/v1/messages", body); got != int64(len(body)) {
		t.Fatalf("the pool holds %d bytes, want %d", got, int64(len(body)))
	}
}

// heldBytes says how many bytes the pool holds while a request of that
// body is in the air.
func heldBytes(t *testing.T, path, body string) int64 {
	t.Helper()
	release := make(chan struct{})
	defer close(release)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	go func() {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		h.gw.ServeHTTP(httptest.NewRecorder(), req)
	}()
	// 等它到了上游：那时候它的身体还占着池子。
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.mu.Lock()
		hits := h.hits
		h.mu.Unlock()
		if hits > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the request never reached the upstream")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.gw.bodies.mu.Lock()
	defer h.gw.bodies.mu.Unlock()
	return h.gw.bodies.used
}

// TestThePoolLetsOneBodyInWhenItIsEmpty says a body that is bigger than
// the whole pool still comes in when the pool is empty.
//
// 配置写小了的时候，那个身体也得能进来 —— 不然它永远等不到，池子空着它
// 也进不来（规格 §5）。
func TestThePoolLetsOneBodyInWhenItIsEmpty(t *testing.T) {
	p := &bodyPool{}
	if !p.take(4<<20, 1<<20) {
		t.Fatal("the pool did not let the body in")
	}
	if p.used != 4<<20 {
		t.Fatalf("used = %d, want %d", p.used, 4<<20)
	}
	// 池子不空了，第二个就进不来了。
	if p.take(1, 1<<20) {
		t.Fatal("the pool let a second body in")
	}
	p.give(4 << 20)
	if p.used != 0 {
		t.Fatalf("used = %d, want 0", p.used)
	}
	if !p.take(1, 1<<20) {
		t.Fatal("the pool did not let the body in after the first one left")
	}
}

// TestThePoolWithNoLimit says a limit of 0 is no limit.
func TestThePoolWithNoLimit(t *testing.T) {
	p := &bodyPool{}
	for i := 0; i < 100; i++ {
		if !p.take(1<<20, 0) {
			t.Fatalf("the pool refused the body number %d", i)
		}
	}
}

// TestTheBodyLimitsSay how big a body may be and how many bytes may be in
// the air at once.
func TestTheBodyLimits(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	if got := h.gw.bodySmallBytes(); got != 4<<20 {
		t.Fatalf("the small limit = %d, want %d (4 MiB)", got, 4<<20)
	}
	if got := h.gw.bodyBudgetBytes(); got != 80<<20 {
		t.Fatalf("the budget = %d, want %d (80 MiB)", got, 80<<20)
	}
	// 配 0 的两样：一个按 1 算，一个是不限。
	h.gw.Cfg.Concurrency.BodySmallMB = 0
	h.gw.Cfg.Concurrency.BodyBudgetMB = 0
	if got := h.gw.bodySmallBytes(); got != 1<<20 {
		t.Fatalf("the small limit = %d, want %d (1 MiB)", got, 1<<20)
	}
	if got := h.gw.bodyBudgetBytes(); got != 0 {
		t.Fatalf("the budget = %d, want 0 (no limit)", got)
	}
}

// TestTheBodyLimitsAreConfigured says the two limits have their defaults.
func TestTheBodyLimitsAreConfigured(t *testing.T) {
	c := config.Default()
	if c.Concurrency.BodySmallMB != 4 {
		t.Errorf("body_small_mb = %d, want 4", c.Concurrency.BodySmallMB)
	}
	if c.Concurrency.BodyBudgetMB != 80 {
		t.Errorf("body_budget_mb = %d, want 80", c.Concurrency.BodyBudgetMB)
	}
}

// TestAnEmptyBodyTakesNoRoom says a body of no bytes does not wait for
// room in the pool.
//
// 零字节的身体不占内存 —— 池子满了也放它进来。
func TestAnEmptyBodyTakesNoRoom(t *testing.T) {
	p := &bodyPool{}
	if !p.take(1<<20, 1<<20) {
		t.Fatal("the pool did not take the bytes")
	}
	if !p.take(0, 1<<20) {
		t.Fatal("the pool did not let an empty body in")
	}
}
