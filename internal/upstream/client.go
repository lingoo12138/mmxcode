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
func defaultTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
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
}

// redirectStatuses are the statuses a redirect can have.
var redirectStatuses = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

// sameHost says whether two URLs are on the same host.
//
// The name is compared without the case, and the port by what it
// means: ":443" on https and no port at all are the same place, but
// another port is another service.
func sameHost(a, b *url.URL) bool {
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

// Do sends a request, following a redirect that stays on the host and
// refusing one that does not.
//
// A redirect to another host is an OtherSiteError: the answer says
// where the request was sent and what to do about it.
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
	// A request that names its own User-Agent keeps it; one that does
	// not says who it is. Without this the account API calls itself
	// Go-http-client.
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	current := req
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
		if !sameHost(next, current.URL) {
			resp.Body.Close()
			return nil, &OtherSiteError{
				From:   current.URL.Host,
				To:     next.Host,
				Status: resp.StatusCode,
			}
		}
		if hop >= 5 {
			return resp, nil
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
func ReadBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return b, nil
}
