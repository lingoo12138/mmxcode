package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mmxcode/internal/store"
)

func TestResolveOrder(t *testing.T) {
	r := New(map[string]string{
		"warp":   "socks5h://warp:4000",
		"direct": "direct",
	}, "http://127.0.0.1:7890")

	cases := []struct {
		name string
		acc  store.Account
		want string
	}{
		{"账号级优先", store.Account{ProxyGroup: "warp", ProxyURL: "http://1.2.3.4:8080"}, "http://1.2.3.4:8080"},
		{"其次出口组", store.Account{ProxyGroup: "warp"}, "socks5h://warp:4000"},
		{"再次全局", store.Account{}, "http://127.0.0.1:7890"},
		{"组名为 direct", store.Account{ProxyGroup: "direct"}, "direct"},
		{"未知组落到全局", store.Account{ProxyGroup: "nope"}, "http://127.0.0.1:7890"},
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
	r := New(nil, "")
	if got := r.Resolve(store.Account{}); got != Direct {
		t.Fatalf("Resolve = %q, want %q", got, Direct)
	}
}

func TestClientIsCachedPerURL(t *testing.T) {
	r := New(nil, "")
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

func TestDirectClientDoesNotUseEnvProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://should-not-be-used:9999")
	r := New(nil, "")
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

func TestHTTPProxyIsUsed(t *testing.T) {
	r := New(nil, "")
	c, err := r.Client("http://127.0.0.1:7890")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Transport.(*http.Transport)
	if tr.Proxy == nil {
		t.Fatal("expected a proxy on the transport")
	}
}

func TestSocks5ClientDialsThroughProxy(t *testing.T) {
	// A tiny SOCKS5 server that answers with a refusal is enough: the
	// point is that the dial goes through the proxy, not that it works.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	r := New(nil, "")
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
	r := New(nil, "http://127.0.0.1:7890")
	if got := r.ResolveForHost(store.Account{}, "127.0.0.1:8080"); got != Direct {
		t.Fatalf("got %q, want direct", got)
	}
	if got := r.ResolveForHost(store.Account{}, "example.com:443"); got != "http://127.0.0.1:7890" {
		t.Fatalf("got %q", got)
	}
}
