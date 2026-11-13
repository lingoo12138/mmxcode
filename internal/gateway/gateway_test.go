package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/config"
	"mmxcode/internal/egress"
	"mmxcode/internal/manager"
	"mmxcode/internal/oauth"
	"mmxcode/internal/rotator"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// harness is a gateway with one account and a fake upstream.
type harness struct {
	srv    *httptest.Server
	gw     *Server
	store  *store.Store
	hits   int
	tokens []string
	// renewals counts the token renewals the gateway asked for.
	renewals int
}

func newHarness(t *testing.T, handler http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == site.TokenPath {
			// 续签也走假服务器：测试永远不碰 MiniMax 自己。
			h.renewals++
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"tok2","refresh_token":"ref2","expires_in":3600}`))
			return
		}
		h.hits++
		h.tokens = append(h.tokens, r.Header.Get("Authorization"))
		handler(w, r)
	}))
	t.Cleanup(h.srv.Close)

	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	now := time.Now()
	s.CreateAccount(context.Background(), store.Account{
		ID: "acc_01", Name: "a", Status: store.StatusActive, UserID: "1234567890",
		CreatedAt: now, UpdatedAt: now,
	})
	s.SaveToken(context.Background(), store.Token{
		AccountID: "acc_01", Access: "tok", Refresh: "ref",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})

	up := upstream.New(upstream.Options{Timeout: 5 * time.Second})
	mgr := manager.New(s, up, account.New(up), egress.New(nil, ""), oauth.NewRenewer())
	// 续签的地址指到假服务器：一个测试碰到续签的时候，也不该去
	// MiniMax 自己那里。
	mgr.TokenURL = h.srv.URL + site.TokenPath
	cfg := config.Default()
	cfg.Server.APIKeys = nil
	h.store = s
	h.gw = New(mgr, cfg, s, nil)
	// 上游地址指到假服务器
	h.gw.UpstreamBase = h.srv.URL
	return h
}

func TestAnthropicPassthrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mavis/api/v1/llm/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`))
	})
	body := `{"model":"MiniMax-M3","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"text":"hi"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestOpenAIPassthrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mavis/api/v1/llm/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[]}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestCodexUsesAnthropicFallback(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// 上游没有 /responses，返回 404
		if r.URL.Path == "/mavis/api/v1/llm/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/mavis/api/v1/llm/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	// 回退路径非流式时返回 Responses 的形状
	if !strings.Contains(w.Body.String(), `"object":"response"`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestQuotaRefusalFailsOverToAnotherAccount(t *testing.T) {
	// The count is a local one: the handler is given before the
	// harness that holds it exists.
	hits := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"message":"insufficient balance"}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 第二个账号
	now := time.Now()
	h.store.CreateAccount(context.Background(), store.Account{
		ID: "acc_02", Name: "b", Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
	})
	h.store.SaveToken(context.Background(), store.Token{
		AccountID: "acc_02", Access: "tok2", Refresh: "ref2",
		ExpiresAt: now.Add(time.Hour), UpdatedAt: now,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if h.hits < 2 {
		t.Fatalf("the request was tried %d times", h.hits)
	}
	// 第一个账号被标记耗尽
	ex, err := h.store.ListExhausted(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].AccountID != "acc_01" {
		t.Fatalf("exhausted = %+v", ex)
	}
}

func TestOtherSiteRedirectIsNotFailover(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer other.Close()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", w.Code)
	}
	if h.hits != 1 {
		t.Fatalf("the request was tried %d times, want 1", h.hits)
	}
	if !strings.Contains(w.Body.String(), "proxy") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("x-api-key", "sk-1")
	w = httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("the key was refused")
	}
}

func TestModels(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"MiniMax-M3"}]}`))
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Object string `json:"object"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Data) == 0 || body.Data[0].ID == "" {
		t.Fatalf("body = %s", w.Body.String())
	}
	if body.Object != "list" {
		t.Fatalf("object = %q", body.Object)
	}
}

func TestModelsFallsBackToBuiltIn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "MiniMax-M3") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestStreamIsForwarded(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {}\n\n"))
		w.Write([]byte("event: message_stop\ndata: {}\n\n"))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "message_start") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
}

// countingStore counts the questions the pool adapter asks the store.
//
// 包一层是为了数问题，不是为了改 store。
type countingStore struct {
	*store.Store
	readings int
}

func (c *countingStore) LatestUsageSnapshots(ctx context.Context) ([]store.UsageSnapshot, error) {
	c.readings++
	return c.Store.LatestUsageSnapshots(ctx)
}

// TestRemainingAllReadsTheCreditsOfEveryAccountAtOnce says the credits
// of a whole list of accounts come from one read.
//
// 一次挑选要问所有候选账号的余额。一次读就够了：每个账号读一次，
// N 个账号就是 N 次读。
func TestRemainingAllReadsTheCreditsOfEveryAccountAtOnce(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_02", "acc_03"} {
		h.store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		})
	}
	// acc_02 的余额读到了，acc_01 没有
	h.store.SaveUsageSnapshot(ctx, store.UsageSnapshot{
		AccountID: "acc_02", HasAmounts: true, Granted: 600, Consumed: 100, Remaining: 500,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	})

	c := &countingStore{Store: h.store}
	p := poolAdapter{c}
	got := p.RemainingAll(ctx, []string{"acc_01", "acc_02", "acc_03"})
	if c.readings != 1 {
		t.Fatalf("the readings were read %d times, want 1", c.readings)
	}
	rem, ok := got["acc_02"]
	if !ok {
		t.Fatalf("the account whose credits are known is not in the map: %+v", got)
	}
	if !rem.Known || rem.Left != 500 {
		t.Fatalf("remaining = %+v", rem)
	}
	if _, ok := got["acc_01"]; ok {
		t.Fatalf("an account whose credits are not known is in the map: %+v", got)
	}

	// 单个账号那条路走的是同一份数据，两边不能对不上
	left, known := p.Remaining(ctx, "acc_02")
	if !known || left != 500 {
		t.Fatalf("remaining = %v, known = %v", left, known)
	}
	if _, known := p.Remaining(ctx, "acc_01"); known {
		t.Fatal("the account without a reading says it has credits")
	}
}

// TestPickReadsTheCreditsOfTheAccountsAtOnce says one pick asks for the
// credits of every account it may choose in one question.
//
// 一个请求要挑账号时，余额是一次读完的，不是一个账号一次。
func TestPickReadsTheCreditsOfTheAccountsAtOnce(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	ctx := context.Background()
	now := time.Now()
	for _, id := range []string{"acc_02", "acc_03"} {
		h.store.CreateAccount(ctx, store.Account{
			ID: id, Name: id, Status: store.StatusActive, CreatedAt: now, UpdatedAt: now,
		})
		h.store.SaveUsageSnapshot(ctx, store.UsageSnapshot{
			AccountID: id, HasAmounts: true, Granted: 600, Remaining: 400,
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		})
	}
	c := &countingStore{Store: h.store}
	rot := rotator.New(poolAdapter{c}, rotator.Options{MaxAttempts: 3, WindowGroup: manager.WindowGroup})
	if _, err := rot.Pick(ctx, "sess-1", "m", map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if c.readings != 1 {
		t.Fatalf("the credits were read %d times for one pick, want 1", c.readings)
	}
}

// TestQuotaRefusalThatNamesTheCreditsSpendsTheWindow says a refusal
// that names the credits spends the account's window.
//
// 说了额度用完就是额度用完：这个账号的窗口记成用完，换一个账号。
func TestQuotaRefusalThatNamesTheCreditsSpendsTheWindow(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"base_resp":{"status_code":1004,"status_msg":"insufficient balance"}}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)

	ex, err := h.store.ListExhausted(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].AccountID != "acc_01" {
		t.Fatalf("exhausted = %+v", ex)
	}
	if ex[0].WindowKey != "general" {
		t.Fatalf("window key = %q, want the group", ex[0].WindowKey)
	}
	if ex[0].Reason != "quota" {
		t.Fatalf("reason = %q", ex[0].Reason)
	}
}

// TestForbiddenThatDoesNotNameTheCreditsDoesNotSpendTheWindow says a
// 403 that only mentions a limit is not a spent account.
//
// 说了"限制"不等于说了"额度用完"。把窗口记成用完，这个账号就白等
// 一整个窗口。
func TestForbiddenThatDoesNotNameTheCreditsDoesNotSpendTheWindow(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"forbidden","detail":"the limit of the requests"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)

	ex, err := h.store.ListExhausted(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 0 {
		t.Fatalf("the window was spent: %+v", ex)
	}
}

// TestTheClientIsToldTheLimitWasReached says the 429 the client gets
// when the credits are gone says the limit was reached.
//
// 上游自己说额度用完了，客户端就该看到这句话：等一会儿再来，而不是
// 以为哪里坏了。
func TestTheClientIsToldTheLimitWasReached(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"base_resp":{"status_code":1004,"status_msg":"insufficient balance"}}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "usage limit reached: insufficient balance") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// TestUnauthorizedRenewsTheToken says a 401 renews the token and asks
// the same account again.
//
// 401 是令牌旧了：续签之后还是这个账号。
func TestUnauthorizedRenewsTheToken(t *testing.T) {
	// 次数是本地的一份：handler 是先给的，那时 h还不存在。
	hits := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"the sign-in was refused"}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if h.renewals != 1 {
		t.Fatalf("the token was renewed %d times, want 1", h.renewals)
	}
	if len(h.tokens) < 2 || h.tokens[0] == h.tokens[1] {
		t.Fatalf("the account was asked again with the same token: %v", h.tokens)
	}
}

// TestForbiddenRenewsTheToken says a 403 renews the token too.
//
// 站点用 403 拒签到，账号这一边也是这么读的：令牌旧了，续签。
func TestForbiddenRenewsTheToken(t *testing.T) {
	// 次数是本地的一份：handler 是先给的，那时 h还不存在。
	hits := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"forbidden"}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if h.renewals != 1 {
		t.Fatalf("the token was renewed %d times, want 1", h.renewals)
	}
	if len(h.tokens) < 2 || h.tokens[0] == h.tokens[1] {
		t.Fatalf("the account was asked again with the same token: %v", h.tokens)
	}
}

// TestRefusedSignInInTheBodyRenewsTheToken says the site's own code for
// a refused sign-in renews the token.
//
// 200 的身体里带着 1000048：站点说签到被拒了。令牌旧了，续签。
func TestRefusedSignInInTheBodyRenewsTheToken(t *testing.T) {
	// 次数是本地的一份：handler 是先给的，那时 h还不存在。
	hits := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.Write([]byte(`{"statusInfo":{"code":1000048,"message":"the sign-in was refused"}}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if h.renewals != 1 {
		t.Fatalf("the token was renewed %d times, want 1", h.renewals)
	}
}

// brokenStream writes one event and then breaks the connection.
//
// 上游断了：客户端要看到一个收尾的事件，而不是一条断掉的流。
func brokenStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Write([]byte("event: message_start\ndata: {}\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	panic(http.ErrAbortHandler)
}

// brokenResponsesStream is a Responses stream that breaks.
func brokenResponsesStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Write([]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	panic(http.ErrAbortHandler)
}

// TestBrokenStreamEndsWithTheProtocolsOwnFailure says a stream that
// breaks ends with one failure event, in the shape of the protocol the
// client speaks.
//
// 断了的流要给一个收尾事件，形状得是客户端认得的那个：OpenAI 的
// 客户端不认 Anthropic 的 error 事件，Codex 的客户端认 response.failed。
func TestBrokenStreamEndsWithTheProtocolsOwnFailure(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		h := newHarness(t, brokenStream)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		body := w.Body.String()
		if n := strings.Count(body, "event: error"); n != 1 {
			t.Fatalf("the stream has %d error events, want 1: %s", n, body)
		}
		if strings.Contains(body, "response.failed") {
			t.Fatalf("the stream has a Responses event in it: %s", body)
		}
	})
	t.Run("openai", func(t *testing.T) {
		h := newHarness(t, brokenStream)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		body := w.Body.String()
		if n := strings.Count(body, `"error"`); n != 1 {
			t.Fatalf("the stream has %d errors, want 1: %s", n, body)
		}
		if !strings.Contains(body, `data: {"error"`) {
			t.Fatalf("the error is not an OpenAI one: %s", body)
		}
		if strings.Contains(body, "event: error") {
			t.Fatalf("the stream has an Anthropic event in it: %s", body)
		}
	})
	t.Run("codex", func(t *testing.T) {
		h := newHarness(t, brokenResponsesStream)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true}`))
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		body := w.Body.String()
		if n := strings.Count(body, "event: response.failed"); n != 1 {
			t.Fatalf("the stream has %d failures, want 1: %s", n, body)
		}
		if strings.Contains(body, "event: error") {
			t.Fatalf("the stream has an Anthropic event in it: %s", body)
		}
		if !strings.Contains(body, `"id":"resp_1"`) {
			t.Fatalf("the failure does not name the response: %s", body)
		}
	})
}

// TestCodexFallbackStreamHasOneFailure says the Codex fallback does not
// end the stream twice.
//
// 上游说不了 Responses 的时候，本地把 Anthropic 的流转成 Responses 的。
// 那一层断了的时候自己会写 response.failed，网关再写一个 Anthropic 的
// error 事件，客户端就看到两次失败，形状还不一样。
func TestCodexFallbackStreamHasOneFailure(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == site.ResponsesPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		brokenStream(w, r)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	body := w.Body.String()
	if n := strings.Count(body, "event: response.failed"); n != 1 {
		t.Fatalf("the stream has %d failures, want 1: %s", n, body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("the stream has an Anthropic event in it: %s", body)
	}
}

// signIn signs the browser in and gives the cookie it got.
func signIn(t *testing.T, h *harness, key string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader("key="+url.QueryEscape(key)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no cookie was given: %v", w.Header()["Set-Cookie"])
	return nil
}

// TestTheCookieIsAccepted says the browser's cookie opens the gateway.
//
// 浏览器不会替页面带上 x-api-key，cookie 是它能带的那张门票。
func TestTheCookieIsAccepted(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	c := signIn(t, h, "sk-1")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.AddCookie(c)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// TestTheSignInPageIsReachableWithoutASession says the page that signs
// the browser in does not need a session.
//
// 没有会话的人正要来这里登录。
func TestTheSignInPageIsReachableWithoutASession(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodGet, "/signin", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), `<form`) {
		t.Fatalf("body = %s", w.Body.String())
	}
	// 页面不回显 key，也不该被缓存
	if strings.Contains(w.Body.String(), "sk-1") {
		t.Fatalf("the key is in the page: %s", w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("cache control = %q", cc)
	}
}

// TestAWrongKeySetsNoCookie says a wrong key is refused and leaves no
// cookie behind.
func TestAWrongKeySetsNoCookie(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader("key=sk-wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatalf("a cookie was given for a wrong key: %+v", c)
		}
	}
}

// TestACookieOfARotatedKeyIsRefused says a cookie stops working when
// the key it was made of is not configured any more.
//
// 换一把 key，旧的 cookie 自己就作废了：服务端不用存任何东西。
func TestACookieOfARotatedKeyIsRefused(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	c := signIn(t, h, "sk-1")
	h.gw.Cfg.Server.APIKeys = []string{"sk-2"}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.AddCookie(c)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
}

// TestTheCookieIsOnlyForTheBrowser says the cookie is HttpOnly and
// Strict.
//
// cookie 本身就是一张门票：谁拿到它谁就能用这个网关。所以脚本读不到
// 它（HttpOnly），别的站点的表单也带不上它（SameSite=Strict）。
func TestTheCookieIsOnlyForTheBrowser(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	c := signIn(t, h, "sk-1")
	if !c.HttpOnly {
		t.Fatal("a script can read the cookie")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("same site = %v, want strict", c.SameSite)
	}
	if c.Path != "/" {
		t.Fatalf("path = %q", c.Path)
	}
	if c.Value == "" || c.Value == "sk-1" {
		t.Fatalf("the cookie carries the key itself: %q", c.Value)
	}
}

// TestAPageWithoutASessionGivesTheSignInPage says a browser that opens
// a page is given the sign-in page.
//
// 浏览器打开的是页面，给它一段 JSON 等于什么也做不了。
func TestAPageWithoutASessionGivesTheSignInPage(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content type = %q, body = %s", ct, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `<form`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// TestAnAPIWithoutASessionGivesJSON says the API clients keep their
// JSON answer.
//
// 脚本和模型客户端读得懂 JSON，读不懂一个登录页。
func TestAnAPIWithoutASessionGivesJSON(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	for _, path := range []string{"/api/usage", "/v1/models"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: code = %d", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("%s: content type = %q", path, ct)
		}
	}
}

// TestWithoutAKeyNothingIsChecked says a gateway without keys lets
// everything through.
//
// 没配 key 的时候网关不查：那时候它只该听在本机，serve 那一条命令管
// 这件事。
func TestWithoutAKeyNothingIsChecked(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[]}`))
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("the request was refused")
	}
}

// TestSignOutTakesTheCookieAway says there is a way out.
//
// 签出要能删掉 cookie：不然浏览器里那张票一直在。
func TestSignOutTakesTheCookieAway(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodDelete, "/api/session", nil)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("the cookie was not taken away: %v", w.Result().Cookies())
	}
}
