package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mmxcode/internal/account"
	"mmxcode/internal/apikey"
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
	srv   *httptest.Server
	gw    *Server
	store *store.Store
	// mu guards the counters below: a test may send many requests at
	// once, and the fake upstream counts them from the server's own
	// goroutines.
	mu     sync.Mutex
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
			h.mu.Lock()
			h.renewals++
			h.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"tok2","refresh_token":"ref2","expires_in":3600}`))
			return
		}
		h.mu.Lock()
		h.hits++
		h.tokens = append(h.tokens, r.Header.Get("Authorization"))
		h.mu.Unlock()
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
		w.Write([]byte(`{"type":"message","id":"msg_1","content":[` +
			`{"type":"text","text":"ok"},` +
			`{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Beijing"}}` +
			`]}`))
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
	var resp struct {
		Object     string `json:"object"`
		Status     string `json:"status"`
		OutputText string `json:"output_text"`
		Output     []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if resp.Object != "response" || resp.Status != "completed" {
		t.Fatalf("the answer = %s", w.Body.String())
	}
	if resp.OutputText != "ok" {
		t.Fatalf("output_text = %q", resp.OutputText)
	}
	// 工具调用不能被丢掉：模型要调工具，客户端得看得见它调的是哪个
	// 工具、拿什么参数去调。非流式这条路以前只留 text 块，tool_use
	// 块一个都没有，模型永远调不了工具。
	var call *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Status    string `json:"status"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	for i := range resp.Output {
		if resp.Output[i].Type == "function_call" {
			call = &resp.Output[i]
		}
	}
	if call == nil {
		t.Fatalf("the tool call was thrown away: %s", w.Body.String())
	}
	if call.CallID != "toolu_1" {
		t.Fatalf("call_id = %q, want the tool_use block's id", call.CallID)
	}
	if call.Name != "get_weather" {
		t.Fatalf("name = %q", call.Name)
	}
	if call.Arguments != `{"city":"Beijing"}` {
		t.Fatalf("arguments = %q", call.Arguments)
	}
	if call.Status != "completed" {
		t.Fatalf("status = %q", call.Status)
	}
	if call.ID == "" {
		t.Fatal("the tool call has no id of its own")
	}
	// 文字块还是那一个文字块：两条路对同一个上游答案要一样。
	var text *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Status    string `json:"status"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	for i := range resp.Output {
		if resp.Output[i].Type == "message" {
			text = &resp.Output[i]
		}
	}
	if text == nil {
		t.Fatalf("the text block was thrown away: %s", w.Body.String())
	}
	if len(text.Content) != 1 || text.Content[0].Text != "ok" {
		t.Fatalf("the message item = %+v", text.Content)
	}
	if text.ID == "" {
		t.Fatal("the message item has no id of its own")
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

	// 错的 key 也是进不去的。
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("x-api-key", "sk-wrong")
	w = httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 for the wrong key", w.Code)
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

// TestForbiddenDoesNotRenewTheToken says a 403 does not renew the
// token.
//
// 聊天这条路只有 401 才续签：403 是站点自己不答，不是令牌旧了。
// 续签一次就是花掉一个只能用一次的 refresh token，不该花在每个
// 403 上。
// TestTheForbiddenIsNotAskedAgain says a 403 that does not name the
// credits is not asked again.
//
// 同一个账号、同一个令牌再问一次，问出来的还是这个答案。参考实现
// 是原样交给客户端的。
func TestTheForbiddenIsNotAskedAgain(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"forbidden"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	h.mu.Lock()
	hits := h.hits
	h.mu.Unlock()
	if hits != 1 {
		t.Fatalf("the upstream was asked %d times, want 1", hits)
	}
}

// TestTheClientIsToldTheUpstreamsOwnWords says the words the client is
// given are the words the upstream said, even when the error is a
// plain string.
//
// 站点答 {"error":"insufficient_balance"} 时，那句话就是上游说的拒绝，
// 客户端该看到它，而不是一句"额度用完了"。
func TestTheClientIsToldTheUpstreamsOwnWords(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"insufficient_balance"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "usage limit reached: insufficient_balance") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestForbiddenDoesNotRenewTheToken(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"forbidden"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if h.renewals != 0 {
		t.Fatalf("the token was renewed %d times, want 0", h.renewals)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
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

// TestAScriptGetsJSONWhenItSignsIn says a script that posts JSON gets
// JSON and the cookie, not a page.
//
// 脚本要的是一张票，不是一页 HTML：跟着重定向走会拿到一页它用不了
// 的东西。
func TestAScriptGetsJSONWhenItSignsIn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(`{"key":"sk-1"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content type = %q, want json", ct)
	}
	if body := w.Body.String(); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("body = %s, want the ok answer", body)
	}
	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no cookie was given: %v", w.Header()["Set-Cookie"])
	}
}

// TestAScriptIsToldInJSONThatItsKeyWasRefused says a refused key is
// refused in JSON when the caller asked in JSON.
//
// 脚本拿到一页 HTML 的话，它读不出那句"key 不对"。
func TestAScriptIsToldInJSONThatItsKeyWasRefused(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(`{"key":"sk-wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content type = %q, want json", ct)
	}
	body := w.Body.String()
	if strings.Contains(body, "<html") {
		t.Fatalf("the answer is a page: %s", body)
	}
	if !strings.Contains(body, "refused") {
		t.Fatalf("body = %s, want the refusal", body)
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

// TestTheNativeFlagIsSafeWhileTheFallbackHappens says the flag that
// says the upstream speaks Responses is read and written by many
// requests at once without a race.
//
// 上游第一次说它没有 /responses 的时候，网关要把这条路关掉：那是一根
// 所有人都读的杆，一根被写一根被读，就是数据竞争。
//
// 这台机器上没有 race detector（-race 要 cgo，这里没有 gcc），所以在
// 这里看不出差别：在有 gcc 的机器上跑
// go test -race ./internal/gateway/
// 才会报出竞争来。
func TestTheNativeFlagIsSafeWhileTheFallbackHappens(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == site.ResponsesPath {
			// 上游没有 Responses：网关要改走本地转换。
			// 慢一点再答，让所有请求都已经在读那根杆了。
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"no such endpoint"}}`))
			return
		}
		w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"hi"}]}`))
	})

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			body := `{"model":"m","input":"hi ` + strconv.Itoa(i) + `"}`
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			w := httptest.NewRecorder()
			h.gw.ServeHTTP(w, req)
		}(i)
	}
	close(start)
	wg.Wait()

	// 第一个 404 把这条路关掉之后，它就一直是关着的。
	if h.gw.NativeResponses.Load() {
		t.Fatal("the native path is still on")
	}

	// 关掉之后，Codex 的请求就走本地转换那条路了：上游再也收不到
	// /responses。
	mu.Lock()
	before := len(paths)
	mu.Unlock()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"again"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths[before:] {
		if p == site.ResponsesPath {
			t.Fatalf("the native path was used again: %v", paths[before:])
		}
	}
}

// TestAnEmptyConfiguredKeyOpensNothing says a configured key that is
// not a key never matches.
//
// 配置里写了空串的时候，一个不带 key 的请求带来的也是空串，两边一比
// 就"相等"了：等于是没有认证。配置这一头要挡住，配置说什么都不能
// 让空 key 进门。
func TestAnEmptyConfiguredKeyOpensNothing(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{""}

	headers := []string{"", " ", "  \t "}
	for _, key := range headers {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("the key %q was accepted: code = %d", key, w.Code)
		}
	}
}

// TestAnEmptyKeyInBearerOpensNothing says the same of the other place
// a key can come from.
//
// Bearer 后面空着，带来的也是空串。
func TestAnEmptyKeyInBearerOpensNothing(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{""}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("the empty bearer key was accepted: code = %d", w.Code)
	}
}

// TestAnEmptyKeySignsNobodyIn says the sign-in page does not give a
// cookie to an empty key either.
//
// 登录是另一扇门：空 key 配在那里的时候，空 key 也要进不去。
func TestAnEmptyKeySignsNobodyIn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{""}

	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader("key="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatalf("a cookie was given for an empty key: %+v", c)
		}
	}
}

// sessionRecorder keeps the session ids the upstream was told about.
//
// 记下发给上游的会话 id：这一节要看的就是它。
type sessionRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (s *sessionRecorder) record(r *http.Request) {
	s.mu.Lock()
	s.ids = append(s.ids, r.Header.Get("X-Mavis-Session-Id"))
	s.mu.Unlock()
}

func (s *sessionRecorder) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

// plainPool is a pool that cannot supply an upstream id.
//
// 没有 UpstreamID 方法的池子：rotator 退回自己算出来的那个 id。
type plainPool struct {
	accounts []store.Account
	sessions map[string]string
}

func (p *plainPool) Accounts(context.Context) ([]store.Account, error) { return p.accounts, nil }

func (p *plainPool) Exhausted(context.Context) ([]store.Exhausted, error) { return nil, nil }

func (p *plainPool) BindSession(_ context.Context, key, accountID string) error {
	if p.sessions == nil {
		p.sessions = map[string]string{}
	}
	p.sessions[key] = accountID
	return nil
}

func (p *plainPool) SessionAccount(_ context.Context, key string) (string, bool, error) {
	id, ok := p.sessions[key]
	return id, ok, nil
}

func (p *plainPool) MarkExhausted(context.Context, string, string, time.Time, string) error {
	return nil
}

func (p *plainPool) Remaining(context.Context, string) (float64, bool) { return 0, false }

// chat sends one chat request and gives the answer back.
func chat(t *testing.T, h *harness, sessionID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	if sessionID != "" {
		req.Header.Set("X-Session-Id", sessionID)
	}
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	return w
}

// TestTheUpstreamIDIsTheOneTheStoreKept says the upstream is told the
// id the database keeps, and not the client's own string.
//
// 上游看到的是数据库里存着的那个 id。客户端自己那串字符串不外发。
func TestTheUpstreamIDIsTheOneTheStoreKept(t *testing.T) {
	rec := &sessionRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	chat(t, h, "client-1", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	seen := rec.seen()
	if len(seen) != 1 {
		t.Fatalf("the upstream was asked %d times", len(seen))
	}
	if seen[0] == "client-1" {
		t.Fatal("the client's own session id went upstream")
	}
	key := rotator.SessionOf(map[string][]string{"X-Session-Id": {"client-1"}}, "", "").Key
	want, err := h.store.UpstreamID(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if seen[0] != want {
		t.Fatalf("upstream id = %q, want the one the store keeps %q", seen[0], want)
	}
}

// TestOneConversationKeepsOneUpstreamID says the two requests of one
// conversation go upstream with the same id.
//
// 一段对话的两次请求，上游看到的是同一个会话 id：重启之后也是这个。
func TestOneConversationKeepsOneUpstreamID(t *testing.T) {
	rec := &sessionRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	chat(t, h, "client-1", body)
	chat(t, h, "client-1", body)

	seen := rec.seen()
	if len(seen) != 2 {
		t.Fatalf("the upstream was asked %d times", len(seen))
	}
	if seen[0] == "" {
		t.Fatal("no upstream id was sent")
	}
	if seen[0] != seen[1] {
		t.Fatalf("the two requests got %q and %q", seen[0], seen[1])
	}
}

// TestTwoConversationsGetDifferentUpstreamIDs says two conversations do
// not share the id the upstream is told about.
//
// 两段对话拿到两个不同的上游 id。
func TestTwoConversationsGetDifferentUpstreamIDs(t *testing.T) {
	rec := &sessionRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	chat(t, h, "client-1", body)
	chat(t, h, "client-2", body)

	seen := rec.seen()
	if len(seen) != 2 {
		t.Fatalf("the upstream was asked %d times", len(seen))
	}
	if seen[0] == "" || seen[1] == "" {
		t.Fatalf("upstream ids = %q and %q", seen[0], seen[1])
	}
	if seen[0] == seen[1] {
		t.Fatalf("the two conversations got the same upstream id %q", seen[0])
	}
}

// TestAPoolWithoutAnUpstreamIDFallsBackToTheDerivedOne says a pool that
// cannot supply an id leaves the derived one in place, and that it is
// stable.
//
// 给不出上游 id 的池子：退回请求自己算出来的那个，两次请求是同一个。
func TestAPoolWithoutAnUpstreamIDFallsBackToTheDerivedOne(t *testing.T) {
	rec := &sessionRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Write([]byte(`{"type":"message","content":[]}`))
	})
	// 换成一个给不出上游 id 的池子。
	h.gw.Rot = rotator.New(&plainPool{accounts: []store.Account{{
		ID: "acc_01", Name: "a", Status: store.StatusActive,
	}}}, rotator.Options{MaxAttempts: 3, WindowGroup: manager.WindowGroup})

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	chat(t, h, "client-1", body)
	chat(t, h, "client-1", body)

	seen := rec.seen()
	if len(seen) != 2 {
		t.Fatalf("the upstream was asked %d times", len(seen))
	}
	// 退回的规则没有变：客户端带了 id，就是客户端自己那串。
	if seen[0] != "client-1" {
		t.Fatalf("upstream id = %q, want the client's own", seen[0])
	}
	if seen[0] != seen[1] {
		t.Fatalf("the two requests got %q and %q", seen[0], seen[1])
	}

	// 客户端没带 id：退回的是第一条消息的哈希。
	chat(t, h, "", body)
	seen = rec.seen()
	want := rotator.SessionOf(nil, "", "hi").UpstreamID
	if seen[len(seen)-1] != want {
		t.Fatalf("upstream id = %q, want the hash %q", seen[len(seen)-1], want)
	}
}

// TestAKeyMadeInTheConsoleOpensTheChatAPI says a key made in the
// console opens the chat API.
//
// 后台申请的那把 key 是给别的机器用的：它得能聊天。
func TestAKeyMadeInTheConsoleOpensTheChatAPI(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	// 配置里有 key，所以门是关着的：这时候进来的那把才真是靠它开的门。
	h.gw.Cfg.Server.APIKeys = []string{"sk-console"}
	plain := makeAKey(t, h.store)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("x-api-key", plain)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("the key made in the console was refused")
	}
}

// TestARevokedKeyDoesNotOpenTheChatAPI says a revoked key opens
// nothing.
//
// 撤销是把门关上：撤了的那一把不该再能进门。
func TestARevokedKeyDoesNotOpenTheChatAPI(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-console"}
	plain := makeAKey(t, h.store)
	if err := h.store.RevokeAPIKey(context.Background(), "key_1", time.Now()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("x-api-key", plain)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401 for a revoked key", w.Code)
	}
}

// TestAKeyMadeInTheConsoleDoesNotOpenTheConsole says a chat key is not
// a console key.
//
// 后台登录用的是另一把 key（配置里那把）：聊天用的 key 不该也能打开
// 管理界面。
func TestAKeyMadeInTheConsoleDoesNotOpenTheConsole(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-console"}
	plain := makeAKey(t, h.store)

	req := httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	req.Header.Set("x-api-key", plain)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401: a chat key opened the console", w.Code)
	}

	// 后台那把 key 还是能进后台。
	req = httptest.NewRequest(http.MethodGet, "/api/usage", nil)
	req.Header.Set("x-api-key", "sk-console")
	w = httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatal("the console key was refused")
	}
}

// makeAKey writes a key down and gives it back in the clear.
func makeAKey(t *testing.T, s *store.Store) string {
	t.Helper()
	plain, err := apikey.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAPIKey(context.Background(), store.APIKey{
		ID: "key_1", Name: "给我的笔记本", KeyHash: apikey.Hash(plain),
		Prefix: apikey.Display(plain), CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return plain
}
