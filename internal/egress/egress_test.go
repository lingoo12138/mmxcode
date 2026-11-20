package egress

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mmxcode/internal/store"
)

func TestResolveOrder(t *testing.T) {
	r := New("http://127.0.0.1:7890")

	cases := []struct {
		name string
		acc  store.Account
		want string
	}{
		{"账号级优先", store.Account{ProxyURL: "http://1.2.3.4:8080"}, "http://1.2.3.4:8080"},
		{"再次全局", store.Account{}, "http://127.0.0.1:7890"},
		{"账号级是 direct", store.Account{ProxyURL: "direct"}, "direct"},
		{"账号级是空白", store.Account{ProxyURL: "   "}, "http://127.0.0.1:7890"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := r.Resolve(c.acc); got != c.want {
				t.Fatalf("Resolve = %q, want %q", got, c.want)
			}
		})
	}
}

func TestResolveEmptyMeansDirect(t *testing.T) {
	r := New("")
	if got := r.Resolve(store.Account{}); got != Direct {
		t.Fatalf("Resolve = %q, want %q", got, Direct)
	}
}

func TestResolveNeverReturnsAnEmptyAddress(t *testing.T) {
	resolvers := []*Resolver{
		New(""),
		New(" "),
		New("direct"),
		New("http://127.0.0.1:7890"),
	}
	for _, r := range resolvers {
		got := r.Resolve(store.Account{})
		if strings.TrimSpace(got) == "" {
			t.Fatalf("Resolve = %q: an empty address would go out direct", got)
		}
	}
}

// 在用的出口就那些：全局默认，加上账号自己写的那些。列那些没人走的地址
// 没有意义 —— 查出来的「可用」什么也不说明。
func TestTheOutletsAreTheOnesInUse(t *testing.T) {
	accounts := []store.Account{
		{ProxyURL: "http://10.0.0.5:1080"},
		{ProxyURL: "http://10.0.0.5:1080"},
		{ProxyURL: "socks5h://10.0.0.6:1080"},
		{},
	}
	outs := Outlets(accounts, map[string][]string{
		store.GlobalGroup: {"http://127.0.0.1:7890"},
	})
	want := []Outlet{
		{Name: DefaultOutlet, URL: "http://127.0.0.1:7890"},
		{Name: OwnOutlet, URL: "http://10.0.0.5:1080"},
		{Name: OwnOutlet, URL: "socks5h://10.0.0.6:1080"},
	}
	if len(outs) != len(want) {
		t.Fatalf("outlets = %+v, want %+v", outs, want)
	}
	for i, o := range outs {
		if o != want[i] {
			t.Fatalf("outlet %d = %+v, want %+v", i, o, want[i])
		}
	}
	urls := OutletURLs(accounts, map[string][]string{
		store.GlobalGroup: {"http://127.0.0.1:7890"},
	})
	if len(urls) != 3 || urls[0] != "http://127.0.0.1:7890" {
		t.Fatalf("urls = %v", urls)
	}
}

// 没有全局默认的时候，在用的就只有账号自己那些地址。
func TestTheOutletsWithoutAGlobalDefault(t *testing.T) {
	outs := Outlets([]store.Account{{ProxyURL: "http://10.0.0.5:1080"}}, map[string][]string{})
	if len(outs) != 1 || outs[0].URL != "http://10.0.0.5:1080" {
		t.Fatalf("outlets = %+v", outs)
	}
}

func TestClientIsCachedPerURL(t *testing.T) {
	r := New("")
	c1, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatal("expected the same client for the same URL")
	}
	c3, err := r.Client("http://127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	if c1 == c3 {
		t.Fatal("expected a different client for a different URL")
	}
}

func TestTheSameAddressWrittenTwoWaysIsOneClient(t *testing.T) {
	r := New("")
	c1, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.Client("HTTP://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatal("expected the same client for the same address")
	}
	c3, err := r.Client("http://127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	c4, err := r.Client("http://127.0.0.1:80")
	if err != nil {
		t.Fatal(err)
	}
	if c3 != c4 {
		t.Fatal("expected the same client for the same address with the default port")
	}
}

func TestInvalidateDropsTheClient(t *testing.T) {
	r := New("")
	c1, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	// 写成另一个样子也要能丢掉同一个。
	r.Invalidate("HTTP://127.0.0.1:7890")
	c2, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	if c1 == c2 {
		t.Fatal("expected a new client after Invalidate")
	}
}

func TestInvalidateClosesTheIdleConnections(t *testing.T) {
	closed := make(chan struct{}, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	srv.Start()
	defer srv.Close()

	r := New("")
	proxyURL := "http://" + srv.Listener.Addr().String()
	c, err := r.Client(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	r.Invalidate(proxyURL)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the idle connection of the dropped transport was not closed")
	}
}

func TestDirectClientDoesNotUseEnvProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://should-not-be-used:9999")
	r := New("")
	c, err := r.Client(Direct)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", c.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("direct transport must not use a proxy")
	}
}

func TestDirectIsComparedWithoutTheCase(t *testing.T) {
	r := New("")
	for _, v := range []string{"direct", "Direct", "DIRECT", " direct "} {
		c, err := r.Client(v)
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		tr, ok := c.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("%q: transport = %T", v, c.Transport)
		}
		if tr.Proxy != nil {
			t.Fatalf("%q: the direct transport must not use a proxy", v)
		}
	}
}

func TestHTTPProxyIsUsed(t *testing.T) {
	r := New("")
	c, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Transport.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatal("expected a proxy on the transport")
	}
}

func TestTheTransportIsTunedForManyAccounts(t *testing.T) {
	r := New("")
	c, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Transport.(*http.Transport)
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("http/2 is not attempted")
	}
	if tr.MaxIdleConnsPerHost != 32 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want 32", tr.MaxIdleConnsPerHost)
	}
}

func TestSocks5ClientDialsThroughProxy(t *testing.T) {
	// A tiny SOCKS5 server that answers with a refusal is enough: the
	// point is that the dial goes through the proxy, not that it works.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	r := New("")
	c, err := r.Client("socks5://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected the dial through the dead proxy to fail")
	}
}

func TestLoopbackNeverProxied(t *testing.T) {
	r := New("http://127.0.0.1:7890")
	if got := r.ResolveForHost(store.Account{}, "127.0.0.1:8080"); got != Direct {
		t.Fatalf("got %q, want direct", got)
	}
	if got := r.ResolveForHost(store.Account{}, "example.com:443"); got != "http://127.0.0.1:7890" {
		t.Fatalf("got %q", got)
	}
}

func TestAnAddressWithoutAHostIsRefused(t *testing.T) {
	r := New("")
	if _, err := r.Client("http://"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := r.Client("socks5://"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestASocks5AddressWithoutAPortIsRefused(t *testing.T) {
	r := New("")
	if _, err := r.Client("socks5://host"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := r.Client("socks5h://host"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestTheCredentialsAreNotInTheErrorText(t *testing.T) {
	r := New("")
	_, err := r.Client("http://user:secret@")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "user") {
		t.Fatalf("the credentials are in the error: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("the error does not show the redacted address: %v", err)
	}
	_, err = r.Client("socks5://user:secret@host")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("the credentials are in the error: %v", err)
	}
}

// TestRedacted says the credentials are taken out of an address that is
// shown, and the rest of it stays.
//
// 地址要给用户看，凭据是秘密：换掉凭据，scheme、主机和端口都还在。
func TestRedacted(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://user:secret@host:1080", "http://***@host:1080"},
		{"socks5h://user:secret@host:1080", "socks5h://***@host:1080"},
		{"http://user@host:1080", "http://***@host:1080"},
		{"http://host:1080", "http://host:1080"},
		{"direct", "direct"},
		{"", ""},
	}
	for _, c := range cases {
		got := Redacted(c.in)
		if got != c.want {
			t.Fatalf("Redacted(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, "user") {
			t.Fatalf("Redacted(%q) = %q, want no credentials", c.in, got)
		}
	}
}

func TestCheckTreatsA407AsAFailure(t *testing.T) {
	// 代理拒绝我们的凭据，就是不能用，不是健康。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer srv.Close()

	r := New("")
	proxyURL := "http://user:secret@" + srv.Listener.Addr().String()
	err := r.Check(context.Background(), proxyURL, "http://127.0.0.1:1/")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "407") {
		t.Fatalf("err = %v, want the status in it", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("the credentials are in the error: %v", err)
	}
}

func TestCheckDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("other"))
	}))
	defer other.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer srv.Close()

	r := New("")
	err := r.Check(context.Background(), Direct, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("the other server got %d requests, want 0", got)
	}
}

func TestCheckHasADeadline(t *testing.T) {
	// 一个接了连接却不回答的代理不能把健康检查挂死。
	old := checkTimeout
	checkTimeout = 300 * time.Millisecond
	defer func() { checkTimeout = old }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	r := New("")
	start := time.Now()
	err = r.Check(context.Background(), Direct, "http://"+ln.Addr().String()+"/")
	if err == nil {
		t.Fatal("expected an error")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the check took %s: it has no deadline of its own", took)
	}
}

func TestCheckPassesWhenTheTargetAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	r := New("")
	if err := r.Check(context.Background(), Direct, srv.URL); err != nil {
		t.Fatal(err)
	}
}
