// Package egress resolves which proxy a request goes out through and
// keeps the HTTP clients that use them.
//
// The resolution follows magpie's model: an account's own address wins,
// then the group it is pointed at, then the global default. "direct"
// means no proxy, and a loopback address never goes through one.
package egress

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"mmxcode/internal/store"
)

// Direct is the value that means "no proxy".
const Direct = "direct"

// Resolver resolves an account's egress and hands out clients for it.
type Resolver struct {
	mu sync.Mutex
	// groups are the named ways out.
	groups map[string]string
	// def is the global way out.
	def string
	// clients are the HTTP clients, by way out.
	clients map[string]*http.Client
	// transports are the transports, by way out.
	transports map[string]http.RoundTripper
}

// New makes a resolver over the configured groups and the global
// default.
func New(groups map[string]string, def string) *Resolver {
	g := make(map[string]string, len(groups))
	for k, v := range groups {
		g[k] = v
	}
	return &Resolver{
		groups:     g,
		def:        def,
		clients:    map[string]*http.Client{},
		transports: map[string]http.RoundTripper{},
	}
}

// Resolve gives the proxy URL an account's requests go through.
//
// Nothing configured means direct: the account has no proxy of its
// own, no group, and there is no global default either.
func (r *Resolver) Resolve(a store.Account) string {
	if u := strings.TrimSpace(a.ProxyURL); u != "" {
		return u
	}
	if g := strings.TrimSpace(a.ProxyGroup); g != "" {
		if u, ok := r.groups[g]; ok {
			return u
		}
	}
	v := strings.TrimSpace(r.def)
	if v == "" {
		return Direct
	}
	return v
}

// ResolveForHost is Resolve for a host, except that a loopback address
// is never proxied.
func (r *Resolver) ResolveForHost(a store.Account, host string) string {
	if isLoopback(host) {
		return Direct
	}
	return r.Resolve(a)
}

// isLoopback says a host is on this machine.
func isLoopback(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Client gives the HTTP client for a proxy URL, making it if it is not
// there yet.
//
// Clients are cached by URL: the proxy is a property of the transport,
// so one URL means one client, however many accounts use it.
func (r *Resolver) Client(proxyURL string) (*http.Client, error) {
	key := strings.TrimSpace(proxyURL)
	if key == "" {
		key = Direct
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clients[key]; ok {
		return c, nil
	}
	tr, err := r.transportLocked(key)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Transport: tr}
	r.clients[key] = c
	return c, nil
}

// Transport gives the transport for a proxy URL, making it if it is not
// there yet.
//
// This is what a caller that has its own client needs: the upstream
// client takes a transport, and the transport is what carries the
// proxy.
//
// Transports are cached by URL: the proxy is a property of the
// transport, so one URL means one transport, however many accounts use
// it.
func (r *Resolver) Transport(proxyURL string) (http.RoundTripper, error) {
	key := strings.TrimSpace(proxyURL)
	if key == "" {
		key = Direct
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.transportLocked(key)
}

// transportLocked is Transport, with the lock held.
func (r *Resolver) transportLocked(key string) (http.RoundTripper, error) {
	if tr, ok := r.transports[key]; ok {
		return tr, nil
	}
	tr, err := newTransport(key)
	if err != nil {
		return nil, err
	}
	r.transports[key] = tr
	return tr, nil
}

// Invalidate drops the cached client for a proxy URL, so a changed
// address is picked up.
func (r *Resolver) Invalidate(proxyURL string) {
	key := strings.TrimSpace(proxyURL)
	if key == "" {
		key = Direct
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, key)
	delete(r.transports, key)
}

// Groups gives the configured groups.
func (r *Resolver) Groups() map[string]string {
	out := make(map[string]string, len(r.groups))
	for k, v := range r.groups {
		out[k] = v
	}
	return out
}

// newTransport makes the transport for a proxy URL.
//
// The SOCKS5 dialer is golang.org/x/net's, not the standard library's:
// the standard one resolves the name here, and a name resolved in
// mainland China may point somewhere else. x/net's sends the name to the
// proxy, which resolves it where the proxy is.
func newTransport(proxyURL string) (*http.Transport, error) {
	base := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	if proxyURL == Direct {
		return base, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy %q: %w", proxyURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		base.Proxy = http.ProxyURL(u)
		return base, nil
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 proxy %q: %w", proxyURL, err)
		}
		ctxDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("socks5 proxy %q: no context dialer", proxyURL)
		}
		base.DialContext = ctxDialer.DialContext
		base.Proxy = nil
		return base, nil
	case "":
		// 没有 scheme 就不是一个地址。默默直连是最坏的一种错：它
		// 看起来跑通了，其实请求根本没走代理。
		return nil, fmt.Errorf("proxy %q: no scheme, write http:// or socks5h://", proxyURL)
	default:
		return nil, fmt.Errorf("proxy %q: unsupported scheme %q", proxyURL, u.Scheme)
	}
}

// Check does one request through a proxy URL and says whether it worked.
//
// It is what the egress health check runs. The target is a small
// address that answers fast; a proxy that cannot reach it is not usable.
func (r *Resolver) Check(ctx context.Context, proxyURL, target string) error {
	c, err := r.Client(proxyURL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("proxy %q: HTTP %d", proxyURL, resp.StatusCode)
	}
	return nil
}
