package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mmxcode/internal/config"
)

// TestAnUpstreamThatIsDownIsNotARateLimit says a 503 from the upstream
// reaches the client as a 503.
//
// 上游说它自己坏了，客户端就该看到上游坏了。把它说成 429，客户端会
// 当成自己被限流：退避，过一会儿再来，再问一个还是坏了的上游。运维
// 看到的也是"被限流了"，而不是"上游挂了"。
func TestAnUpstreamThatIsDownIsNotARateLimit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"message":"the upstream is down"}`))
	})
	// 一次就够：这里要看的不是重试，是最后给客户端的那个状态。
	h.gw.Cfg.Rotation.MaxAttempts = 1

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503, body = %s", w.Code, w.Body.String())
	}
}

// TestAnUpstreamBadRequestIsNotARateLimit says a 400 from the upstream
// reaches the client as a 400.
//
// 400 是上游说"这个请求不对"。说成 429 是骗客户端：它会把这个请求
// 一直重试下去，而重试一个不对的请求没有用。
func TestAnUpstreamBadRequestIsNotARateLimit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"the request is not right"}}`))
	})
	h.gw.Cfg.Rotation.MaxAttempts = 1

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

// TestAQuotaRefusalIsStillARateLimit says the one refusal that stays a
// 429 is the one about the credits.
//
// 额度用完了就是限流：客户端等一会儿再来是对的，所以这一条还是 429，
// 而且要带上限流的样子，让客户端知道该等而不是该换账号。
func TestAQuotaRefusalIsStillARateLimit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"base_resp":{"status_code":1004,"status_msg":"insufficient balance"}}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429, body = %s", w.Code, w.Body.String())
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if body.Error.Type != "rate_limit_error" {
		t.Fatalf("the error's type = %q, want rate_limit_error", body.Error.Type)
	}
	if !strings.Contains(body.Error.Message, "usage limit reached") {
		t.Fatalf("the error's message = %q", body.Error.Message)
	}
}

// TestATransportErrorIsABadGateway says a request that never got
// through is the gateway's own problem.
//
// 网络没通的时候上游没有给过任何状态。这时候没有上游的话可以转述，
// 那是网关这一边的事：502。把它说成 429，客户端会以为是自己被限流
// 了，而真正坏着的是网关到上游那一段路。
func TestATransportErrorIsABadGateway(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	// 上游指到一个已经关掉的地方：连都连不上。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	h.gw.UpstreamBase = deadURL
	h.gw.Cfg.Rotation.MaxAttempts = 1

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502, body = %s", w.Code, w.Body.String())
	}
}

// TestABodyIsNotReadWhileTheGatewayIsBusy says a request that is
// waiting for a free slot has not read its body yet.
//
// 闸门是网关对外面承诺的并发：在这个数里面，请求才占着网关的内存。请求
// 体最大 64 MiB —— 排在一百个请求后面的每一个都已经把身体读进内存了的
// 话，那是 6.4 GB，跟承诺的并发一点关系都没有。排队的时候身体还在客
// 户端那边，网关没有替它占内存。
func TestABodyIsNotReadWhileTheGatewayIsBusy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	// 等不了多久：这道测试要的是「身体没被读」，不是「等到了」。
	h.gw.Cfg.Concurrency.Wait = config.Duration(200 * time.Millisecond)
	// 把闸门占满：承诺的那个名额没有了。
	limit := h.gw.concurrencyLimit(context.Background())
	for i := 0; i < limit; i++ {
		if !h.gw.gate.take(limit) {
			t.Fatalf("the gate gave out only %d of %d slots", i, limit)
		}
	}
	body := &countedBody{Reader: strings.NewReader(`{"model":"m"}`)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want 429, body = %s", w.Code, w.Body.String())
	}
	if body.read != 0 {
		t.Fatalf("the body was read %d bytes while the gateway was busy, want 0", body.read)
	}
}

// countedBody counts how many bytes were read out of it.
//
// 它是那道测试的眼睛：身体被读了多少，看得见。
type countedBody struct {
	*strings.Reader
	read int
}

func (c *countedBody) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.read += n
	return n, err
}

func (c *countedBody) Close() error { return nil }

// TestABodyOverTheLimitIsRefusedWithoutBeingRead says a body that
// declares itself too big is turned away without being read.
//
// 客户端说了身体有多大（Content-Length）：知道了就不用读 —— 读进来是 64
// MiB 的内存，而它马上就是一个 413。
func TestABodyOverTheLimitIsRefusedWithoutBeingRead(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	body := &countedBody{Reader: strings.NewReader(`{"model":"m"}`)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.ContentLength = maxBodyBytes + 1
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413, body = %s", w.Code, w.Body.String())
	}
	if body.read != 0 {
		t.Fatalf("the body was read %d bytes, want 0", body.read)
	}
	if h.hits != 0 {
		t.Fatalf("the upstream was asked %d times, want 0", h.hits)
	}
}

// TestTheTooBigMessageSaysTheLimit says the client is told how big the
// limit is, and how big it sent.
//
// 传图片的人被挡下来的时候，得知道那个上限是多少、自己超了多少，才知道
// 该少传一张还是该改配置。一句「太大」是什么也没说。
func TestTheTooBigMessageSaysTheLimit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	body := &countedBody{Reader: strings.NewReader(`{"model":"m"}`)}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	req.ContentLength = maxBodyBytes + 1
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413, body = %s", w.Code, w.Body.String())
	}
	msg := w.Body.String()
	if !strings.Contains(msg, fmt.Sprintf("%d", maxBodyBytes)) {
		t.Errorf("the message does not say the limit (%d): %s", maxBodyBytes, msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("%d", maxBodyBytes+1)) {
		t.Errorf("the message does not say how big the body was: %s", msg)
	}
}

// TestABodyOverTheLimitIsRefused says a body that is too big is turned
// away instead of being read into memory.
//
// 没有上限的时候，一个请求就能把它这个网关的内存吃光：这里读的是整
// 个身子，读进来就一直拿着。
func TestABodyOverTheLimitIsRefused(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	body := &manyBytes{left: maxBodyBytes + 1}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code = %d, want 413, body = %s", w.Code, w.Body.String())
	}
	if h.hits != 0 {
		t.Fatalf("the upstream was asked %d times, want 0", h.hits)
	}
}

// manyBytes is a body of n bytes that does not hold them: it makes them
// as they are read.
//
// 64 MiB 的身体用 strings.Repeat 造的话，那条测试自己就拿着 64 MiB，再加
// 读身体时的那一份 —— 测试自己比它测的东西还胖。这个不占那份内存。
type manyBytes struct {
	left int
}

func (m *manyBytes) Read(p []byte) (int, error) {
	if m.left <= 0 {
		return 0, io.EOF
	}
	if len(p) > m.left {
		p = p[:m.left]
	}
	for i := range p {
		p[i] = 'x'
	}
	m.left -= len(p)
	return len(p), nil
}

func (m *manyBytes) Close() error { return nil }

// TestTheRequestBodyLimitIs64MiB says how large a request body may be.
//
// 64 MiB：用户是可能传图片的 —— 站点的模型收 text、image、video（参考
// 插件的模型名单就是这么写的），而图片是 base64 写在身体里的，一张图几
// MB 是常态。而 1M token 的对话是 4 MB 上下 —— 64 MiB 是它的十几倍。
func TestTheRequestBodyLimitIs64MiB(t *testing.T) {
	if maxBodyBytes != 64<<20 {
		t.Fatalf("maxBodyBytes = %d, want %d (64 MiB)", maxBodyBytes, 64<<20)
	}
}

// TestTheModelListIsReadOnceFromTheSite says the model list is the
// site's own, and that asking for it costs one request.
//
// 站点的模型接口给的是 provider config 的样子，不是模型列表的样子：
// 要读它得照着那个样子读（internal/account 的 ParseModels）。问的时候
// 不带令牌 —— 带上有可能在路上把令牌续了，一份模型名单不值得换一把
// 新令牌。
//
// 名单一年也换不了几回，所以读到的那份留着：客户端问十回，站点被问一
// 回。答得看不懂的时候（这里答的是模型列表的样子，不是 provider
// config 的样子）摆的还是内置的那一份。
func TestTheModelListIsReadOnceFromTheSite(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"id":"whatever"}]}`))
	})
	var last *httptest.ResponseRecorder
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()
		h.gw.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("code = %d", w.Code)
		}
		last = w
	}
	if h.hits != 1 {
		t.Fatalf("the upstream was asked %d times, want 1", h.hits)
	}
	w := last
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if body.Object != "list" {
		t.Fatalf("object = %q", body.Object)
	}
	if len(body.Data) == 0 {
		t.Fatalf("the list is empty: %s", w.Body.String())
	}
	for _, m := range body.Data {
		if m.ID == "whatever" {
			t.Fatalf("the upstream's answer was used: %s", w.Body.String())
		}
	}
	if !strings.Contains(w.Body.String(), "MiniMax-M3") {
		t.Fatalf("the built-in list is not in the answer: %s", w.Body.String())
	}
}

// TestTheUpstreamRefusalDoesNotEndTheSignIn says a refusal from the
// upstream does not tell magpie that the sign-in is gone.
//
// magpie 的规矩：401 默认就把这个账号标成「签入失效」，用户被踢回签入
// 页 —— 除非回答里带 X-Magpie-Sign-In: kept。
//
// 网关后面所有账号同时死掉的那一次，客户端收到的是上游的 401。magpie 会
// 把「网关这个账号」（用户签入的那个）标成签入失效，用户重新签进去发现
// 一切正常 —— 网关是好的，坏的是网关后面的账号。参考插件在每个上游回答
// 上都带着这个头（minimax/index.mjs 里 answer 那一句）。
func TestTheUpstreamRefusalDoesNotEndTheSignIn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"forbidden"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Magpie-Sign-In"); got != "kept" {
		t.Errorf("the sign-in header = %q, want %q", got, "kept")
	}
}

// TestTheUpstreamUnauthorizedDoesNotEndTheSignIn says the same of a 401
// from the upstream.
//
// 上游说令牌旧了：网关续签一次，续签之后还是被拒，这个账号先放着，换一
// 个。全部账号都这样的时候，客户端最后拿到的是上游的 401 —— 那时候
// magpie 会以为签入没了，而签入其实好好的。
func TestTheUpstreamUnauthorizedDoesNotEndTheSignIn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"the sign-in was refused"}`))
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Magpie-Sign-In"); got != "kept" {
		t.Errorf("the sign-in header = %q, want %q", got, "kept")
	}
}

// TestTheRefusedKeyStillEndsTheSignIn says the gateway's own refusal is
// not excused.
//
// 客户端自己的 key 被拒，那是网关这扇门挡的：那把 key 不对，签入就是不行。
// magpie 应该照旧把这个账号记成签入失效，所以那个头不能加在这个 401 上。
func TestTheRefusedKeyStillEndsTheSignIn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	h.gw.Cfg.Server.APIKeys = []string{"sk-1"}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Magpie-Sign-In"); got != "" {
		t.Errorf("the sign-in header = %q, want none: the key really was refused", got)
	}
}

// TestABackendErrorDoesNotEndTheSignIn says the gateway's own failures
// carry no sign-in header.
//
// 网关自己没通（502）：那跟签入没关系，也不需要说什么 —— 说「签入还在」
// 反而会盖住一件该被看见的事。
func TestABackendErrorDoesNotEndTheSignIn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	// 上游指到一个已经关掉的地方：连都连不上。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	h.gw.UpstreamBase = deadURL
	h.gw.Cfg.Rotation.MaxAttempts = 1

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502, body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Magpie-Sign-In"); got != "" {
		t.Errorf("the sign-in header = %q, want none", got)
	}
}

// TestTheBackoffStopsWhenTheClientIsGone says the wait between two
// attempts ends when the client goes away.
//
// 客户端走了之后再睡上半分钟是白等：上游的次数是白花的，网关的连接
// 是白占的。退避要看着请求的命。
func TestTheBackoffStopsWhenTheClientIsGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// 客户端走了：上游还在答，答了也没人听。
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	start := time.Now()
	h.gw.ServeHTTP(w, req)
	elapsed := time.Since(start)
	if h.hits != 1 {
		t.Fatalf("the upstream was asked %d times, want 1", h.hits)
	}
	// 第一次退避是半秒：没有停住的话，这里至少要等半秒。
	if elapsed > 400*time.Millisecond {
		t.Fatalf("the request took %s: the backoff did not stop", elapsed)
	}
}
