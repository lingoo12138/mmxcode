package upstream

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"mmxcode/internal/site"
)

// ChatRequest is one chat request to MiniMax.
//
// The body is the client's own, unchanged: MiniMax Code speaks the
// Anthropic and the OpenAI format natively, so nothing is rewritten
// on the way in.
type ChatRequest struct {
	// BaseURL is the host to talk to.
	BaseURL string
	// Path is the endpoint's path.
	Path string
	// Access is the account's access token.
	Access string
	// SessionID is the session the upstream should treat this as.
	SessionID string
	// TZOffset is the client's timezone offset in seconds.
	TZOffset int
	// Body is the request body.
	Body []byte
	// Stream says the answer is a stream.
	Stream bool
	// Extra holds headers the client set, copied over.
	Extra map[string]string
}

// Chat sends a chat request.
//
// transport may be nil, in which case the client's own is used. The
// account's exit is a property of the transport, so an account with
// its own proxy passes its own client here.
func (c *Client) Chat(ctx context.Context, req ChatRequest, transport http.RoundTripper) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(req.BaseURL, "/")+req.Path, strings.NewReader(string(req.Body)))
	if err != nil {
		return nil, err
	}
	for k, v := range req.Extra {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.Access)
	// MiniMax Code gives its SDK a placeholder key; the real one is in
	// Authorization.
	httpReq.Header.Set("x-api-key", site.PlaceholderKey)
	httpReq.Header.Set("User-Agent", site.UserAgent)
	httpReq.Header.Set("X-Mavis-Agent-Id", site.AgentID)
	httpReq.Header.Set("X-Mavis-Timezone-Offset", strconv.Itoa(req.TZOffset))
	if req.SessionID != "" {
		httpReq.Header.Set("X-Mavis-Session-Id", req.SessionID)
	}
	// MiniMax Code's own requests all carry the version header, so
	// every request here carries it too — the site reads the request
	// as MiniMax Code wrote it, not as the client's protocol wrote it.
	if httpReq.Header.Get("anthropic-version") == "" {
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	}
	// 必须：不带压缩时 MiniMax 才会一个事件一个事件地吐
	httpReq.Header.Set("accept-encoding", "identity")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	// A nil transport means the client's own: the client was made with
	// the transport of the account's exit, and falling back to a fresh
	// default one would quietly drop the proxy.
	if transport == nil {
		transport = c.http.Transport
	}
	resp, err := DoWith(httpReq, transport)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Models asks the site for its models.
//
// The token is used when there is one; the site also answers without.
func (c *Client) Models(ctx context.Context, baseURL, access string) ([]byte, error) {
	u := strings.TrimRight(baseURL, "/") + site.ModelsPath + "?" + site.ModelsQuery
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", site.UserAgent)
	httpReq.Header.Set("accept-encoding", "identity")
	if access != "" {
		httpReq.Header.Set("Authorization", "Bearer "+access)
	}
	resp, err := DoWith(httpReq, c.http.Transport)
	if err != nil {
		return nil, err
	}
	body, err := ReadBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("models: HTTP %d", resp.StatusCode)
	}
	return body, nil
}
