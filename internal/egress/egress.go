// Package egress resolves which proxy a request goes out through and
// keeps the HTTP clients that use them.
//
// The resolution follows magpie's model: an account's own address wins,
// then the group it is pointed at, then the global default. "direct"
// means no proxy, and a loopback address never goes through one.
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
	// groups are the named ways out. They are set when the resolver is
	// made and are not written to afterwards.
	groups map[string]string
	// def is the global way out. Like the groups, it is set when the
	// resolver is made and is not written to afterwards.
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
//
// A group whose value is empty is not a way out: it falls through to
// the global default. An empty value that resolved to nothing would
// send the traffic out direct, which is the one mistake a proxy
// support must not make.
//
// The lock is taken, so that the maps are read the same way the caches
// are.
func (r *Resolver) Resolve(a store.Account) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if u := strings.TrimSpace(a.ProxyURL); u != "" {
		return u
	}
	if g := strings.TrimSpace(a.ProxyGroup); g != "" {
		if u, ok := r.groups[g]; ok {
			if v := strings.TrimSpace(u); v != "" {
				return v
			}
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

// Groups gives the configured groups.
//
// The lock is taken, so that the maps are read the same way the caches
// are.
func (r *Resolver) Groups() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.groups))
	for k, v := range r.groups {
		out[k] = v
	}
	return out
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
