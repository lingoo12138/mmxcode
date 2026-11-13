// Package upstream is the HTTP client for MiniMax's own hosts.
//
// It does three things the standard client does not:
//
//   - it refuses a redirect that leaves the host, because MiniMax's
//     international site answers a mainland-China network with a 307 to
//     the China site, whose tokens do not work on these;
//   - it checks the business codes MiniMax puts in the body of a 200;
//   - it asks for no compression, because MiniMax answers a chat it is
//     asked to compress as one gzip member flushed at the end, which
//     turns a streamed answer into one lump.
//
// It refuses two more redirects for the same kind of reason: one that
// downgrades https to http, because the sign-in would go out in
// cleartext, and one that changes the path or the query of a signed
// request, because the signature is made over the path and the query
// and cannot be replayed.
package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Options configures a client.
type Options struct {
	// Timeout is the whole-request timeout.
	Timeout time.Duration
	// UserAgent is what the client calls itself.
	UserAgent string
	// Transport is used when given; the client makes its own otherwise.
	Transport http.RoundTripper
}

// Client talks to MiniMax's hosts.
type Client struct {
	http      *http.Client
	timeout   time.Duration
	userAgent string
}

// New makes a client.
func New(opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "MiniMaxCode"
	}
	transport := opts.Transport
	if transport == nil {
		transport = defaultTransport()
	}
	return &Client{
		// The timeout is not on the shared client but on the request
		// this client makes: a chat streams for as long as it needs and
		// goes through its own client, while the account API and the
		// token endpoint answer in seconds and must not hang.
		http:      &http.Client{Transport: transport},
		timeout:   timeout,
		userAgent: ua,
	}
}

// defaultTransport is the transport used when the caller has none.
//
// It asks for no compression and does not add Accept-Encoding of its
// own: MiniMax's behaviour with compression is worse than useless for a
// stream.
//
// Many accounts share one transport, so it is tuned for that: HTTP/2 is
// negotiated, and up to 32 idle connections per host are kept. Without
// that, a third request to the same host would have its connection
// closed instead of pooled.
func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
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
}

// redirectStatuses are the statuses a redirect can have.
var redirectStatuses = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

// maxBody is the most a response body may be.
const maxBody = 32 << 20

// sameHost says whether two URLs are on the same host.
//
// The name is compared without the case, and the port by what it
// means: ":443" on https and no port at all are the same place, but
// another port is another service.
//
// The scheme has to be the same too. A redirect that keeps the host
// but changes https to http would send the Authorization header out
// in cleartext, and whoever is on the way could read it and replay it.
func sameHost(a, b *url.URL) bool {
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	if !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return effectivePort(a) == effectivePort(b)
}

// effectivePort is the port a URL really uses.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// sameAddress says whether two URLs are on the same host and port,
// whatever the scheme is.
func sameAddress(a, b *url.URL) bool {
	if !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return a.Port() == b.Port()
}

// sameRequest says whether a redirect keeps the path and the query.
//
// The signature of a signed request is computed over the path and the
// query, so a redirect that changes either of them cannot carry the
// old signature.
func sameRequest(a, b *url.URL) bool {
	return a.Path == b.Path && a.RawQuery == b.RawQuery
}

// signed says whether a request carries a signature.
//
// The two headers are the ones internal/sign makes: "yy" over the
// path, the query and the body, and "x-signature" over the time and
// the body.
func signed(h http.Header) bool {
	return h.Get("yy") != "" || h.Get("x-signature") != ""
}

// copyRequest makes the request the redirect loop sends.
//
// The caller's request is left as it is: the copy is what the
// User-Agent goes into, and a request with no header map at all is
// given an empty one so that writing into it cannot panic.
func copyRequest(req *http.Request) *http.Request {
	out := req.Clone(req.Context())
	if out.Header == nil {
		out.Header = http.Header{}
	}
	return out
}

// isDowngrade says whether a redirect takes the request from https to
// http.
func isDowngrade(from, to *url.URL) bool {
	return strings.EqualFold(from.Scheme, "https") && strings.EqualFold(to.Scheme, "http")
}

// schemeHost is what an address is called in an error: the scheme and
// the host, without the path.
func schemeHost(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}

// Do sends a request, following a redirect that stays on the host and
// refusing one that does not.
//
// A redirect to another host is an OtherSiteError: the answer says
// where the request was sent and what to do about it. A redirect that
// downgrades the scheme is a SchemeDowngradeError, and one that would
// replay a signature made for another path is a StaleSignatureError.
// None of them is followed.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	// The redirect handling is done here, not by http.Client, so the
	// cross-host case can be told apart from the same-host one.
	client := &http.Client{
		Transport: c.http.Transport,
		Timeout:   c.timeout,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// The caller's request is not touched: the User-Agent goes into a
	// copy, and a request with no header map at all is given one, so
	// writing into it cannot panic.
	current := copyRequest(req)
	// A request that names its own User-Agent keeps it; one that does
	// not says who it is. Without this the account API calls itself
	// Go-http-client.
	if current.Header.Get("User-Agent") == "" {
		current.Header.Set("User-Agent", c.userAgent)
	}
	for hop := 0; ; hop++ {
		resp, err := client.Do(current)
		if err != nil {
			return nil, err
		}
		loc := resp.Header.Get("Location")
		if loc == "" || !redirectStatuses[resp.StatusCode] {
			return resp, nil
		}
		next, err := current.URL.Parse(loc)
		if err != nil {
			return resp, nil
		}
		if isDowngrade(current.URL, next) && sameAddress(current.URL, next) {
			resp.Body.Close()
			return nil, &SchemeDowngradeError{
				From:   schemeHost(current.URL),
				To:     schemeHost(next),
				Status: resp.StatusCode,
			}
		}
		if !sameHost(next, current.URL) {
			resp.Body.Close()
			return nil, &OtherSiteError{
				From:   current.URL.Host,
				To:     next.Host,
				Status: resp.StatusCode,
			}
		}
		if signed(current.Header) && !sameRequest(current.URL, next) {
			// The signature is over the path and the query. Sending the
			// old one to another path is a request the upstream refuses,
			// and that refusal reads as a dead token — a false one that
			// costs a refresh token. The redirect is refused instead.
			resp.Body.Close()
			return nil, &StaleSignatureError{
				From:   schemeHost(current.URL),
				To:     schemeHost(next),
				Status: resp.StatusCode,
			}
		}
		if hop >= 5 {
			// Too many redirects is an error, not an answer: handing
			// back the redirect itself would leave the caller with a
			// body nobody read.
			resp.Body.Close()
			return nil, fmt.Errorf("too many redirects")
		}
		// 307 and 308 keep the method and the body; the others turn a
		// non-GET into a GET without a body.
		//
		// The body has to be read again: http.Request.Body is a reader
		// the previous request already consumed, so replaying it as it
		// is would send an empty body — and a signed request with the
		// wrong body is refused. GetBody is what reads it a second time;
		// http.NewRequest prepares it for bytes.Reader and
		// strings.Reader. Without it the body cannot be replayed, so the
		// answer is handed back as it came.
		method, body, length := current.Method, current.Body, current.ContentLength
		replay := false
		if resp.StatusCode != 307 && resp.StatusCode != 308 {
			if method != http.MethodGet && method != http.MethodHead {
				method, body, length = http.MethodGet, nil, 0
			}
		} else if body != nil {
			if current.GetBody == nil {
				return resp, nil
			}
			fresh, err := current.GetBody()
			if err != nil {
				return resp, nil
			}
			body = fresh
			replay = true
		}
		resp.Body.Close()
		nextReq, err := http.NewRequestWithContext(current.Context(), method, next.String(), body)
		if err != nil {
			return nil, err
		}
		nextReq.Header = current.Header.Clone()
		if replay {
			// The body came out of GetBody, which hands out a plain
			// reader, and http.NewRequest only recognises bytes and
			// strings readers. Without the length the replay would go
			// out with no Content-Length at all, chunked instead of
			// what the first request had.
			if length != 0 {
				nextReq.ContentLength = length
			}
			// The body is a reader now, so the next hop would not know
			// how to read it again. Keep the way to do it.
			nextReq.GetBody = current.GetBody
		}
		current = nextReq
	}
}

// Get sends a GET.
func (c *Client) Get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	return c.Do(req)
}

// PostForm sends a form POST, as the OAuth endpoints want.
func (c *Client) PostForm(ctx context.Context, rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	return c.Do(req)
}

// PostJSON sends a JSON POST.
func (c *Client) PostJSON(ctx context.Context, rawURL string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	return c.Do(req)
}

// ReadBody reads a response's body and closes it.
//
// The body is read through a limit: an upstream that streams without
// end must not be able to take all the memory. A body over the limit
// is an error, not a truncated answer.
func ReadBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("read body: no body")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(b) > maxBody {
		return nil, fmt.Errorf("read body: the body is larger than %d bytes", maxBody)
	}
	return b, nil
}
