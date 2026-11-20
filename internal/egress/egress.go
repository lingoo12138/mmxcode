// Package egress resolves which proxy a request goes out through and
// keeps the HTTP clients that use them.
//
// The resolution has two steps: an account's own address wins, then
// the global default. Nothing configured means direct, and a loopback
// address never goes through a proxy.
//
// Both "socks5://" and "socks5h://" resolve the name at the proxy,
// not here: the address is sent to the proxy as it is written, and the
// proxy looks it up where the proxy is. That is what curl's "socks5h://"
// does; curl's "socks5://" resolves the name locally, and this package
// does not do that.
package egress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"mmxcode/internal/store"
)

// Direct is the value that means "no proxy".
const Direct = "direct"

// checkTimeout is how long a health check waits for its answer.
//
// It is a variable so that the test can shorten it.
var checkTimeout = 10 * time.Second

// Resolver resolves an account's egress and hands out clients for it.
type Resolver struct {
	mu sync.Mutex
	// def is the global way out. It is set when the resolver is made
	// and is not written to afterwards.
	def string
	// groups are the outlet groups, by name: the addresses of each, in
	// the order they are tried.
	groups map[string][]string
	// health says what the last probe of an address said.
	health map[string]bool
	// clients are the HTTP clients, by way out.
	clients map[string]*http.Client
	// transports are the transports, by way out.
	transports map[string]http.RoundTripper
}

// New makes a resolver over the global default.
func New(def string) *Resolver {
	return &Resolver{
		def:        def,
		groups:     map[string][]string{},
		health:     map[string]bool{},
		clients:    map[string]*http.Client{},
		transports: map[string]http.RoundTripper{},
	}
}

// Resolve gives the proxy URL an account's requests go through.
//
// Nothing configured means direct: the account has no proxy of its
// own, and there is no global default either.
//
// The lock is taken, so that the maps are read the same way the caches
// are.
func (r *Resolver) Resolve(a store.Account) string {
	c := r.Candidates(a)
	if len(c) == 0 {
		return Direct
	}
	return c[0]
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
// so one URL means one client, however many accounts use it. The key is
// the normalised URL, so the same address written two ways is one
// client.
func (r *Resolver) Client(proxyURL string) (*http.Client, error) {
	key := cacheKey(proxyURL)
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
	key := cacheKey(proxyURL)
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
//
// The dropped transport's idle connections are closed: they belong to
// the old address and would otherwise stay open until they time out.
func (r *Resolver) Invalidate(proxyURL string) {
	key := cacheKey(proxyURL)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, key)
	if tr, ok := r.transports[key]; ok {
		delete(r.transports, key)
		if t, ok := tr.(*http.Transport); ok {
			t.CloseIdleConnections()
		}
	}
}

// Outlet is one address that traffic goes out through.
type Outlet struct {
	// Name is what the page calls it.
	Name string
	// URL is the address.
	URL string
}

// The names the outlets page gives the two kinds of outlet.
const (
	// DefaultOutlet is the global default.
	DefaultOutlet = "全局默认"
	// OwnOutlet is an address an account carries itself.
	OwnOutlet = "账号自带"
)

// Outlets gives the addresses that are in use.
//
// The groups come first — the global one, then the others by name — and
// then the addresses the accounts carry themselves, each of them once.
//
// 在用的出口就这些：全局组、每个组，加上账号自己写的那些。健康检查查的就
// 是它们，页面列的也是它们 —— 列那些没人走的地址，查出来的「可用」什么
// 也不说明。
func Outlets(accounts []store.Account, groups map[string][]string) []Outlet {
	var out []Outlet
	seen := map[string]bool{}
	add := func(name, url string) {
		u := strings.TrimSpace(url)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, Outlet{Name: name, URL: u})
	}
	// 全局组先来：它是所有人都跟着的那一个。
	for _, u := range groups[store.GlobalGroup] {
		add(DefaultOutlet, u)
	}
	names := make([]string, 0, len(groups))
	for n := range groups {
		if n == store.GlobalGroup {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, u := range groups[n] {
			add(n, u)
		}
	}
	for _, a := range accounts {
		add(OwnOutlet, a.ProxyURL)
	}
	return out
}

// OutletURLs gives the addresses of the outlets, in order.
func OutletURLs(accounts []store.Account, groups map[string][]string) []string {
	outs := Outlets(accounts, groups)
	urls := make([]string, 0, len(outs))
	for _, o := range outs {
		urls = append(urls, o.URL)
	}
	return urls
}

// cacheKey is the key a proxy URL is cached under.
//
// The same address written two ways is the same address: the scheme is
// lowercased, the host is lowercased, the default port is dropped, and
// the rest is trimmed. Without this, "HTTP://h:1" and "http://h:1"
// would be two clients, and Invalidate would drop only one of them.
func cacheKey(proxyURL string) string {
	s := strings.TrimSpace(proxyURL)
	if s == "" {
		return Direct
	}
	if strings.EqualFold(s, Direct) {
		return Direct
	}
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	u.Host = host
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	return u.String()
}

// isDirect says whether a value means "no proxy".
//
// The comparison is without the case and without the space around it:
// "Direct" and "DIRECT" mean the same thing as "direct".
func isDirect(proxyURL string) bool {
	return strings.EqualFold(strings.TrimSpace(proxyURL), Direct)
}

// Redacted is a proxy URL as it may be shown.
//
// The credentials are taken out: they are a secret, and the health
// check's error is stored in the database. The scheme, the host and
// the port stay, so that one address can still be told from another.
//
// An address without a scheme has no credentials to take out of it, so
// it is left as it is.
func Redacted(proxyURL string) string {
	s := strings.TrimSpace(proxyURL)
	i := strings.Index(s, "://")
	if i < 0 {
		return s
	}
	rest := s[i+3:]
	authority, tail := rest, ""
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		authority, tail = rest[:slash], rest[slash:]
	}
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return s
	}
	return s[:i+3] + "***@" + authority[at+1:] + tail
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
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	if isDirect(proxyURL) {
		return base, nil
	}
	u, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil {
		return nil, fmt.Errorf("parse proxy %q: %w", Redacted(proxyURL), err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy %q: no host, write an address like http://host:port", Redacted(proxyURL))
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		base.Proxy = http.ProxyURL(u)
		return base, nil
	case "socks5", "socks5h":
		if u.Port() == "" {
			return nil, fmt.Errorf("proxy %q: no port, write an address like socks5h://host:port", Redacted(proxyURL))
		}
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("socks5 proxy %q: %w", Redacted(proxyURL), err)
		}
		ctxDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("socks5 proxy %q: no context dialer", Redacted(proxyURL))
		}
		base.DialContext = ctxDialer.DialContext
		base.Proxy = nil
		return base, nil
	case "":
		// 没有 scheme 就不是一个地址。默默直连是最坏的一种错：它
		// 看起来跑通了，其实请求根本没走代理。
		return nil, fmt.Errorf("proxy %q: no scheme, write http:// or socks5h://", Redacted(proxyURL))
	default:
		return nil, fmt.Errorf("proxy %q: unsupported scheme %q", Redacted(proxyURL), u.Scheme)
	}
}

// Check does one request through a proxy URL and says whether it worked.
//
// It is what the egress health check runs. The target is a small
// address that answers fast; a proxy that cannot reach it is not usable.
//
// The check has its own deadline, so a proxy that takes the connection
// and never answers fails the check instead of hanging it.
func (r *Resolver) Check(ctx context.Context, proxyURL, target string) error {
	c, err := r.Client(proxyURL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept-Encoding", "identity")
	// A redirect means the target is not usable as a health probe, so it
	// is not followed: the answer has to come from the target itself.
	client := &http.Client{
		Transport: c.Transport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The body is drained so the connection goes back into the pool.
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	// A redirect is not followed: the answer has to come from the target
	// itself, and a redirect says the target is somewhere else. It is
	// not a failure either — the proxy did carry the request through.
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("proxy %q: HTTP %d", Redacted(proxyURL), resp.StatusCode)
	}
	return nil
}
