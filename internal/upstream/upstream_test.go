package upstream

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFollowsSameHostRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("arrived"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	resp, err := c.Get(context.Background(), srv.URL+"/start")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "arrived" {
		t.Fatalf("body = %q", body)
	}
}

func TestSameHostRedirectKeepsTheBody(t *testing.T) {
	// 307 之后重放，body 必须还在。签名是按 body 算的，body 没了，
	// 上游看到的就是一个签错的请求。
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if len(bodies) == 1 {
			w.Header().Set("Location", "/again")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/start", strings.NewReader(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(bodies) != 2 {
		t.Fatalf("got %d requests, want 2: %v", len(bodies), bodies)
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("the replayed body differs: %q vs %q", bodies[0], bodies[1])
	}
	if bodies[1] != `{"a":1}` {
		t.Fatalf("the replayed body = %q", bodies[1])
	}
}

func TestRefusesCrossHostRedirect(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("other"))
	}))
	defer other.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	_, err := c.Get(context.Background(), srv.URL+"/start")
	if err == nil {
		t.Fatal("expected an error")
	}
	var oe *OtherSiteError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v (%T), want OtherSiteError", err, err)
	}
	if oe.Status != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d", oe.Status)
	}
	// 另一个服务器一个请求都没有收到：跟随了就收到了。
	if got := hits.Load(); got != 0 {
		t.Fatalf("the other server got %d requests, want 0", got)
	}
}

func TestRefusesSchemeDowngrade(t *testing.T) {
	// https 转到同一个 host 的 http：Authorization 就会明文发出去。
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "http://"+r.Host+"/x")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// The transport speaks plaintext whatever the scheme says, so a
	// downgrade can be tried without a certificate.
	base := &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	c := New(Options{Timeout: 5 * time.Second, Transport: &plainTransport{base: base}})
	_, err := c.Get(context.Background(), "https://"+srv.Listener.Addr().String()+"/start")
	if err == nil {
		t.Fatal("expected an error")
	}
	var de *SchemeDowngradeError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v (%T), want SchemeDowngradeError", err, err)
	}
	if de.Status != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d", de.Status)
	}
	if !strings.Contains(de.Error(), "http") {
		t.Fatalf("the error does not say the scheme was downgraded: %v", de)
	}
	// 只收到了第一个请求：降级后的地址没有被访问过。
	if got := hits.Load(); got != 1 {
		t.Fatalf("the server got %d requests, want 1", got)
	}
}

// plainTransport talks plaintext whatever the scheme of the request
// says.
type plainTransport struct {
	base *http.Transport
}

func (t *plainTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	out := r.Clone(r.Context())
	out.URL.Scheme = "http"
	return t.base.RoundTrip(out)
}

func TestRefusesRedirectThatWouldReplayAStaleSignature(t *testing.T) {
	// 签名是按 path+query 算的。改了 path 还把旧的签名带过去，
	// 上游会拒签，那看起来就是一个死 token，白白花掉一个刷新令牌。
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/other")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("yy", "deadbeef")
	req.Header.Set("x-signature", "cafebabe")
	_, err = c.Do(req)
	if err == nil {
		t.Fatal("expected an error")
	}
	var se *StaleSignatureError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want StaleSignatureError", err, err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("the server got %d requests, want 1", got)
	}
}

func TestFollowsRedirectThatKeepsThePathAndTheSignature(t *testing.T) {
	// path 和 query 都没变，签名还是对的，这种重定向照跟。
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if hits.Load() == 1 {
			w.Header().Set("Location", "/start?x=1")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/start?x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("yy", "deadbeef")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := hits.Load(); got != 2 {
		t.Fatalf("the server got %d requests, want 2", got)
	}
}

func TestRefusesRedirectThatChangesTheQueryOfASignedRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if hits.Load() == 1 {
			w.Header().Set("Location", "/start?other=1")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/start?x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-signature", "cafebabe")
	_, err = c.Do(req)
	if err == nil {
		t.Fatal("expected an error")
	}
	var se *StaleSignatureError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want StaleSignatureError", err, err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("the server got %d requests, want 1", got)
	}
}

func TestTooManyRedirectsIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/loop")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})
	resp, err := c.Get(context.Background(), srv.URL+"/loop")
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err = %v", err)
	}
}

func TestDoDoesNotTouchTheCallersRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	c := New(Options{Timeout: 5 * time.Second})

	// 一个没有 header 的请求不能 panic。
	bare, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	bare.Header = nil
	resp, err := c.Do(bare)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 调用方的 header map 里不能被塞进 User-Agent。
	var got string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Write([]byte("ok"))
	}))
	defer srv2.Close()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv2.URL+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != "MiniMaxCode" {
		t.Fatalf("the server saw the user-agent %q", got)
	}
	if req.Header.Get("User-Agent") != "" {
		t.Fatalf("the caller's request was changed: %q", req.Header.Get("User-Agent"))
	}
}

func TestReadBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ReadBody(resp)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("body = %q", b)
	}
}

func TestReadBodyWithNoResponse(t *testing.T) {
	if _, err := ReadBody(nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestReadBodyRefusesAVeryLongBody(t *testing.T) {
	// 一个不停往里写的上游不能把内存吃光。
	body := io.NopCloser(io.LimitReader(filler{}, MaxBody()+1024))
	resp := &http.Response{Body: body}
	if _, err := ReadBody(resp); err == nil {
		t.Fatal("expected an error")
	}
}

// filler is a reader that never ends.
type filler struct{}

func (filler) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestTheDefaultTransportIsTunedForManyAccounts(t *testing.T) {
	tr := defaultTransport()
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("http/2 is not attempted")
	}
	if tr.MaxIdleConnsPerHost != 32 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 32", tr.MaxIdleConnsPerHost)
	}
}

func TestCheckEnvelopeBusinessError(t *testing.T) {
	// HTTP 200 but the body says it failed. The message in the body is
	// not the one the 1000048 case makes up, so the two can be told
	// apart.
	raw := []byte(`{"statusInfo":{"code":1000048,"message":"account not found"}}`)
	err := CheckEnvelope("check-in", raw)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrSignInRefused) {
		t.Fatalf("err = %v, want ErrSignInRefused", err)
	}
	var be *BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v (%T), want *BusinessError", err, err)
	}
	if be.Code != 1000048 {
		t.Fatalf("code = %d", be.Code)
	}
	if be.What != "check-in" {
		t.Fatalf("what = %q", be.What)
	}
	// The wording is the one the code has always produced.
	if err.Error() != "check-in: the sign-in was refused" {
		t.Fatalf("err = %q", err.Error())
	}
}

func TestCheckEnvelopeBusinessErrorCarriesTheCode(t *testing.T) {
	raw := []byte(`{"statusInfo":{"code":1001,"message":"something went wrong"}}`)
	err := CheckEnvelope("credits", raw)
	if err == nil {
		t.Fatal("expected an error")
	}
	var be *BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v (%T), want *BusinessError", err, err)
	}
	if be.Code != 1001 {
		t.Fatalf("code = %d", be.Code)
	}
	if be.Message != "something went wrong" {
		t.Fatalf("message = %q", be.Message)
	}
	if err.Error() != "credits: something went wrong" {
		t.Fatalf("err = %q", err.Error())
	}
	// 不是签到被拒，就不是 ErrSignInRefused。
	if errors.Is(err, ErrSignInRefused) {
		t.Fatalf("err = %v, want not ErrSignInRefused", err)
	}
}

func TestCheckEnvelopeBaseResp(t *testing.T) {
	raw := []byte(`{"base_resp":{"status_code":1001,"status_msg":"nope"}}`)
	err := CheckEnvelope("credits", raw)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
	var be *BusinessError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v (%T), want *BusinessError", err, err)
	}
	if be.Code != 1001 {
		t.Fatalf("code = %d", be.Code)
	}
}

func TestCheckEnvelopeOK(t *testing.T) {
	raw := []byte(`{"base_resp":{"status_code":0},"statusInfo":{"code":0},"data":{}}`)
	if err := CheckEnvelope("credits", raw); err != nil {
		t.Fatal(err)
	}
}

func TestCheckEnvelopeNotJSON(t *testing.T) {
	if err := CheckEnvelope("credits", []byte("<html>")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestQuotaRefusalDetection(t *testing.T) {
	cases := map[string]bool{
		`{"error":{"message":"insufficient balance"}}`:   true,
		`{"message":"额度不足"}`:                             true,
		`{"message":"积分已用完"}`:                            true,
		`{"message":"model not available"}`:              false,
		`{"message":"quota exceeded"}`:                   true,
		`{"message":"rate limit exceeded"}`:              false,
		`{"message":"too many requests"}`:                false,
		`{"message":"rate-limited"}`:                     false,
		`{"message":"rate limited by upstream"}`:         false,
		`{"message":"rate_limit exceeded"}`:              false,
		`{"message":"throttled"}`:                        false,
		`{"message":"request throttled by the gateway"}`: false,
	}
	for body, want := range cases {
		if got := IsQuotaRefusal([]byte(body)); got != want {
			t.Errorf("IsQuotaRefusal(%s) = %v, want %v", body, got, want)
		}
	}
}

// TestTheBodyIsRefusedWhenItIsTooBig says a body over the limit is an
// error, not a truncated body.
//
// 截断的回答是半个回答：客户端拿着它当完整的用，比报错糟得多。
func TestTheBodyIsRefusedWhenItIsTooBig(t *testing.T) {
	body := io.NopCloser(strings.NewReader(strings.Repeat("x", 100)))
	_, err := ReadBodyLimit(body, 10)
	if err == nil {
		t.Fatal("the too big body was taken")
	}
	var tooBig *TooLargeError
	if !errors.As(err, &tooBig) {
		t.Fatalf("the error = %v, want the one that says the body is too big", err)
	}
	if tooBig.Limit != 10 {
		t.Errorf("the limit in the error = %d, want 10", tooBig.Limit)
	}
}

// TestTheLimitIsOneForTheWholeProcess says the limit is one number for
// the whole process, and that a zero does not take it away.
//
// 「没有上限」不是一个选项：那个洞正是这么来的。
func TestTheLimitIsOneForTheWholeProcess(t *testing.T) {
	t.Cleanup(func() { SetMaxBody(DefaultMaxBody) })
	SetMaxBody(1234)
	if MaxBody() != 1234 {
		t.Errorf("the limit = %d, want 1234", MaxBody())
	}
	SetMaxBody(0)
	if MaxBody() != 1234 {
		t.Errorf("the limit = %d after a zero, want 1234", MaxBody())
	}
}
