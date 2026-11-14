// Package gateway serves the chat protocols and the management API.
//
// A chat request is bound to an account by its session, and stays
// there. When the account is spent, another is tried and the request
// is replayed — but only while nothing has been written to the client
// yet.
//
// The session id the upstream is told about is the one the database
// keeps: an opaque id, made once and kept, so the upstream sees one
// conversation and the mapping from the session to the upstream's
// conversation survives a restart. The client's own session id does not
// go upstream, and neither does a hash of the request's words.
//
// What that does not fix: two conversations whose system prompt and
// first user message are the same, sent by a client that sends no
// session id of its own, are one session by the session key's own
// definition. They share one upstream id, and one upstream
// conversation, however random that id is, and no server can tell them
// apart: there is nothing in the request that does. The fix for that
// case is the client sending X-Session-Id.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"mmxcode/internal/config"
	"mmxcode/internal/convert"
	"mmxcode/internal/manager"
	"mmxcode/internal/rotator"
	"mmxcode/internal/site"
	"mmxcode/internal/store"
	"mmxcode/internal/upstream"
)

// nowOffset gives the timezone offset in seconds east of UTC.
//
// It is the value the reference sends as getTimezoneOffset() * -60.
// time.Time has no Offset() method: Zone()'s second value is it.
func nowOffset() int {
	_, offset := time.Now().Zone()
	return offset
}

// Server is the gateway.
type Server struct {
	// Mgr holds the accounts.
	Mgr *manager.Manager
	// Cfg is the configuration.
	Cfg config.Config
	// Store is the database.
	Store *store.Store
	// Rot picks the account.
	Rot *rotator.Rotator
	// UpstreamBase is the host the chats go to. It is a field so a
	// test can point it at a stand-in.
	UpstreamBase string
	// NativeResponses says the upstream speaks the Responses protocol
	// itself. It is tried and turned off when the upstream says no.
	//
	// 每个请求都读它，而第一个 404 会把它写掉：一个字段被一个请求写、
	// 被别的请求读，就是数据竞争。所以它是 atomic 的。
	NativeResponses atomic.Bool
	// WebUI serves the management pages, when there is one.
	WebUI http.Handler
}

// New makes a gateway.
func New(mgr *manager.Manager, cfg config.Config, s *store.Store, webui http.Handler) *Server {
	g := &Server{
		Mgr:          mgr,
		Cfg:          cfg,
		Store:        s,
		UpstreamBase: site.LLMHost,
		WebUI:        webui,
	}
	// 原生路径先开着：上游说它没有 /responses 的时候再关。
	g.NativeResponses.Store(true)
	g.Rot = rotator.New(poolAdapter{s}, rotator.Options{
		MaxAttempts: cfg.Rotation.MaxAttempts,
		Cooldown:    cfg.Rotation.ExhaustedCooldown.Duration(),
		WindowGroup: manager.WindowGroup,
		Strategy:    cfg.Rotation.Strategy,
	})
	return g
}

// poolStore is what the pool adapter reads from the store.
//
// It is an interface and not the store itself, so that a test can count
// the questions the adapter asks without the store knowing about
// counting.
type poolStore interface {
	ListAccounts(ctx context.Context) ([]store.Account, error)
	ListExhausted(ctx context.Context) ([]store.Exhausted, error)
	BindSession(ctx context.Context, sessionKey, accountID string) error
	SessionAccount(ctx context.Context, sessionKey string) (string, bool, error)
	UpstreamID(ctx context.Context, sessionKey string) (string, error)
	SetExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error
	LatestUsageSnapshots(ctx context.Context) ([]store.UsageSnapshot, error)
}

// poolAdapter makes the store into a rotator pool.
type poolAdapter struct{ s poolStore }

func (p poolAdapter) Accounts(ctx context.Context) ([]store.Account, error) {
	return p.s.ListAccounts(ctx)
}

func (p poolAdapter) Exhausted(ctx context.Context) ([]store.Exhausted, error) {
	return p.s.ListExhausted(ctx)
}

func (p poolAdapter) BindSession(ctx context.Context, key, accountID string) error {
	return p.s.BindSession(ctx, key, accountID)
}

func (p poolAdapter) SessionAccount(ctx context.Context, key string) (string, bool, error) {
	return p.s.SessionAccount(ctx, key)
}

// UpstreamID gives the upstream id of a session.
//
// The id is the one the database keeps: an opaque id, made once and
// kept, so the upstream sees one conversation and the mapping survives
// a restart. The client's own session id does not go upstream.
func (p poolAdapter) UpstreamID(ctx context.Context, key string) (string, error) {
	return p.s.UpstreamID(ctx, key)
}

func (p poolAdapter) MarkExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error {
	return p.s.SetExhausted(ctx, accountID, windowKey, until, reason)
}

// RemainingAll gives the credits left of a whole list of accounts.
//
// The rotator asks for every account it may pick in one question, so
// the answer comes out of one read of the readings and not one read
// per account.
//
// The map holds only the accounts whose credits are known: an account
// that is not in it has not been read yet, and the rotator takes it
// after the ones whose balance it knows.
func (p poolAdapter) RemainingAll(ctx context.Context, accountIDs []string) map[string]rotator.Remaining {
	out := map[string]rotator.Remaining{}
	if len(accountIDs) == 0 {
		return out
	}
	wanted := make(map[string]bool, len(accountIDs))
	for _, id := range accountIDs {
		wanted[id] = true
	}
	snaps, err := p.s.LatestUsageSnapshots(ctx)
	if err != nil {
		return out
	}
	for _, s := range snaps {
		if !wanted[s.AccountID] {
			continue
		}
		if !s.HasAmounts {
			// 没读到余额的账号不算有余额：留空，让 rotator 把它排在后面。
			continue
		}
		out[s.AccountID] = rotator.Remaining{Left: s.Remaining, Known: true}
	}
	return out
}

// Remaining gives one account's credits left.
//
// It goes through the same read as RemainingAll, so that the two
// cannot disagree about one account.
func (p poolAdapter) Remaining(ctx context.Context, accountID string) (float64, bool) {
	rem, ok := p.RemainingAll(ctx, []string{accountID})[accountID]
	if !ok {
		return 0, false
	}
	return rem.Left, true
}

// ServeHTTP routes a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/signin":
		// 登录页不用会话：没有会话的人正要来这里登录。
		s.signInPage(w, r, http.StatusOK, "")
		return
	case "/api/session":
		// 发 cookie 和销 cookie 都不用会话。
		s.session(w, r)
		return
	}
	if !s.auth(r) {
		if isAPIPath(r.URL.Path) {
			writeError(w, http.StatusUnauthorized, "the key was refused")
			return
		}
		// 浏览器来的是页面：给它登录页，不是一段 JSON。
		s.signInPage(w, r, http.StatusOK, "")
		return
	}
	switch r.URL.Path {
	case "/v1/messages":
		s.handleChat(w, r, protocolAnthropic)
		return
	case "/v1/chat/completions":
		s.handleChat(w, r, protocolOpenAI)
		return
	case "/v1/responses":
		s.handleChat(w, r, protocolCodex)
		return
	case "/v1/models":
		s.handleModels(w, r)
		return
	}
	if s.WebUI != nil && strings.HasPrefix(r.URL.Path, "/api/") {
		s.WebUI.ServeHTTP(w, r)
		return
	}
	if s.WebUI != nil {
		s.WebUI.ServeHTTP(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "no such endpoint")
}

// protocol is the protocol the client speaks.
type protocol int

const (
	protocolAnthropic protocol = iota
	protocolOpenAI
	protocolCodex
)

// handleChat serves a chat request.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, proto protocol) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the request body could not be read")
		return
	}
	// 上游请求的时限就是配的那一个。流式对话也吃这份预算：想要更长的
	// 就把 upstream.timeout 调大。
	ctx := r.Context()
	if d := s.Cfg.Upstream.Timeout.Duration(); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	model := modelOf(body)
	stream := isStream(body)
	sess := rotator.SessionOf(r.Header, systemOf(body), firstUserOf(body))
	// 发给上游的会话 id 是数据库里存着的那个：不重算，也不把客户端
	// 自己那串字符串发出去。取不到就是取不到，不能拿别的顶上：顶上来的
	// 那个上游没见过，会把这段对话当成新的一段。
	upstreamID, err := s.Rot.UpstreamID(ctx, sess)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	used := map[string]bool{}
	var lastErr error
	// quota 说最后一次失败是“额度用完了”，quotaMsg 是上游自己说的那句。
	// 客户端最后看到的是上游的话，不是网关自己编的一句。
	var quota bool
	var quotaMsg string
	attempts := s.Cfg.Rotation.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
accountLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		plan, err := s.Rot.Pick(ctx, sess.Key, model, used)
		if err != nil {
			if lastErr != nil {
				s.writeFailure(w, quota, quotaMsg, lastErr)
				return
			}
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		used[plan.AccountID] = true

		tok, err := s.Mgr.Token(ctx, plan.AccountID)
		if err != nil {
			lastErr = err
			continue
		}
		client, _, err := s.Mgr.Client(ctx, plan.AccountID)
		if err != nil {
			lastErr = err
			continue
		}

		upBody, upPath, convertBack := s.upstreamShape(body, proto)
		chatReq := upstream.ChatRequest{
			BaseURL:   s.UpstreamBase,
			Path:      upPath,
			Access:    tok.Access,
			SessionID: upstreamID,
			// 秒，UTC 以东。参考实现发的是 getTimezoneOffset() * -60，
			// 和 Zone() 给的偏移是同一个值。time.Time 没有 Offset()
			// 方法，Zone() 的第二个返回值就是它。
			TZOffset:  nowOffset(),
			Body:      upBody,
			Stream:    stream,
			Anthropic: upPath == site.MessagesPath,
			Extra:     clientHeaders(r),
		}

		// renewed 说这个账号的令牌已经续过一次：MiniMax 的 refresh token
		// 只能用一次，续第二次会把账号签死。
		renewed := false
		// 同一个账号可以再问几次：限流、5xx、续签之后，都是这个账号
		// 再来一次就好。
		for try := 0; try < attempts; try++ {
			resp, raw, err := s.attempt(ctx, chatReq, client.Transport, stream)
			if err != nil {
				var oe *upstream.OtherSiteError
				if errors.As(err, &oe) {
					// 跨站：不换号、不消耗 refresh token
					writeError(w, http.StatusBadGateway, oe.Error())
					return
				}
				// 网络没通：还是这个账号，等一等再来。
				lastErr = err
				time.Sleep(backoff(try))
				continue
			}

			// 上游没有 /responses：改用本地转换，同一个账号再来一次
			if resp.StatusCode == http.StatusNotFound && proto == protocolCodex && s.NativeResponses.Load() {
				s.NativeResponses.Store(false)
				upBody, upPath, convertBack = s.upstreamShape(body, proto)
				chatReq.Body = upBody
				chatReq.Path = upPath
				chatReq.Anthropic = upPath == site.MessagesPath
				resp, raw, err = s.attempt(ctx, chatReq, client.Transport, stream)
				if err != nil {
					var oe *upstream.OtherSiteError
					if errors.As(err, &oe) {
						writeError(w, http.StatusBadGateway, oe.Error())
						return
					}
					lastErr = err
					time.Sleep(backoff(try))
					continue
				}
			}

			// 200 且是流：直接转出去
			if resp.StatusCode == http.StatusOK && raw == nil {
				s.writeStream(w, resp, proto, convertBack, model)
				return
			}

			// 200 但身体里有业务错误
			if resp.StatusCode == http.StatusOK {
				if err := upstream.CheckEnvelope("chat", raw); err == nil {
					s.writeJSON(w, resp, raw, proto, convertBack)
					return
				}
			}

			failure := s.failure(resp.StatusCode, raw)
			switch s.action(failure) {
			case rotator.ActionFailover:
				// 额度用完了：这个账号的窗口记成用完，换一个账号。
				until := s.windowReset(ctx, plan.AccountID, plan.GroupKey)
				_ = s.Rot.MarkWindowExhausted(ctx, plan.AccountID, plan.GroupKey, until, "quota")
				lastErr = fmt.Errorf("the account's credits or plan are used up")
				quota = true
				quotaMsg = failure.Message
				continue accountLoop
			case rotator.ActionRenew:
				if renewed {
					// 续过一次还是被拒：这个账号先放着，换一个。
					lastErr = fmt.Errorf("the sign-in was refused (HTTP %d)", resp.StatusCode)
					quota = false
					quotaMsg = ""
					continue accountLoop
				}
				newTok, rerr := s.Mgr.ForceRenew(ctx, plan.AccountID)
				if rerr != nil {
					// 跨站：不换号、不消耗 refresh token
					var oe *upstream.OtherSiteError
					if errors.As(rerr, &oe) {
						writeError(w, http.StatusBadGateway, oe.Error())
						return
					}
					lastErr = rerr
					quota = false
					quotaMsg = ""
					continue accountLoop
				}
				// 用新 token 再来一次，还是这个账号
				renewed = true
				chatReq.Access = newTok.Access
				quota = false
				quotaMsg = ""
				continue
			case rotator.ActionRetrySame:
				// 限流、5xx：这个账号等一等，再问它一次。
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				quota = false
				quotaMsg = ""
				time.Sleep(backoff(try))
				continue
			default:
				// 给不出别的说法：上游的答案原样给客户端。
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(resp.StatusCode)
				w.Write(raw)
				return
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("the request could not be served")
	}
	s.writeFailure(w, quota, quotaMsg, lastErr)
}

// failure says what went wrong upstream.
//
// The code and the message come out of the parsed answer, so that the
// decision the rotator makes and the words the client is given are read
// from one thing and not from two.
func (s *Server) failure(status int, raw []byte) rotator.Failure {
	f := rotator.Failure{Status: status, Body: raw}
	err := upstream.CheckEnvelope("chat", raw)
	if err == nil {
		return f
	}
	var be *upstream.BusinessError
	if !errors.As(err, &be) {
		return f
	}
	f.Code = be.Code
	f.Message = be.Message
	// 1000048：站点说签到被拒了。令牌照旧了，账号本身没问题。
	if errors.Is(err, upstream.ErrSignInRefused) {
		f.Refused = true
	}
	return f
}

// action says what to do about the answer.
//
// The rotator owns the rule. The one thing it cannot know is that the
// site refuses a sign-in with a 403 as well as with a 401 — the account
// package reads both that way — so the gateway says the sign-in was
// refused.
//
// It says it only when the rotator did not read the answer as a
// refusal to spend: an answer that names the credits is about the
// credits, and spending the window is the right answer for it.
func (s *Server) action(failure rotator.Failure) rotator.Action {
	action := rotator.Decide(failure)
	if action == rotator.ActionFailover {
		return action
	}
	if failure.Status != http.StatusUnauthorized && failure.Status != http.StatusForbidden {
		return action
	}
	failure.Refused = true
	return rotator.Decide(failure)
}

// writeFailure writes the answer of a request that could not be served.
//
// The words the client is given are the upstream's own when it said the
// credits are gone: "usage limit reached: …" tells the client to wait
// and try again, which a plain error does not.
func (s *Server) writeFailure(w http.ResponseWriter, quota bool, quotaMsg string, err error) {
	if quota {
		message := quotaMsg
		if message == "" {
			message = "the account's credits or plan are used up"
		}
		writeRateLimit(w, message)
		return
	}
	writeError(w, http.StatusTooManyRequests, err.Error())
}

// attempt sends the request upstream and gives the answer back.
//
// A stream is read no further than its first event: enough to tell an
// error from an answer, little enough that the client still gets the
// first token as soon as the upstream sends it. The rest of the stream
// is left on the response's body.
//
// raw is the whole body when the answer was not a stream, and nil when
// it was.
func (s *Server) attempt(ctx context.Context, req upstream.ChatRequest, transport http.RoundTripper, wantStream bool) (*http.Response, []byte, error) {
	resp, err := s.Mgr.Up.Chat(ctx, req, transport)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, raw, nil
	}
	if !wantStream {
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return resp, nil, err
		}
		return resp, raw, nil
	}
	br := bufio.NewReader(resp.Body)
	head, _ := peekEvent(br)
	if sseHasError(head) {
		// 200 里装着错误：整条读完，当成失败处理
		rest, _ := io.ReadAll(br)
		resp.Body.Close()
		return resp, append(head, rest...), nil
	}
	// 把读走的第一个事件放回去，客户端一个字节都不会少
	resp.Body = &rejoinedBody{
		Reader: io.MultiReader(bytes.NewReader(head), br),
		Closer: resp.Body,
	}
	return resp, nil, nil
}

// rejoinedBody is a body whose first event was read ahead of time.
type rejoinedBody struct {
	io.Reader
	io.Closer
}

// peekEvent reads the first event of a stream.
//
// An SSE event ends at the blank line, so that is what is read.
func peekEvent(br *bufio.Reader) ([]byte, error) {
	var head []byte
	for {
		line, err := br.ReadString('\n')
		head = append(head, line...)
		if err != nil {
			return head, err
		}
		if line == "\n" {
			return head, nil
		}
	}
}

// sseHasError says a stream's first event carries an error.
//
// MiniMax can answer 200 with an error in the stream. By the time the
// client has seen a byte the account cannot be changed any more, so
// the error is looked for before anything is written.
func sseHasError(head []byte) bool {
	for _, line := range strings.Split(string(head), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev struct {
			Type       string          `json:"type"`
			Error      json.RawMessage `json:"error"`
			StatusInfo *struct {
				Code int `json:"code"`
			} `json:"statusInfo"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if ev.StatusInfo != nil && ev.StatusInfo.Code != 0 {
			return true
		}
		if len(ev.Error) > 0 && string(ev.Error) != "null" && string(ev.Error) != "{}" {
			return true
		}
		if ev.Type == "error" {
			return true
		}
	}
	return false
}

// upstreamShape gives the request the upstream is to get.
//
// Anthropic and OpenAI go through untouched: the site speaks both. The
// Responses protocol is turned into the Anthropic one, unless the site
// has shown it speaks Responses itself.
func (s *Server) upstreamShape(body []byte, proto protocol) (out []byte, path string, convertBack bool) {
	switch proto {
	case protocolAnthropic:
		return body, site.MessagesPath, false
	case protocolOpenAI:
		return body, site.ChatCompletionsPath, false
	default:
		if s.NativeResponses.Load() {
			return body, site.ResponsesPath, false
		}
		b, err := convert.ToAnthropic(body)
		if err != nil {
			return body, site.ResponsesPath, false
		}
		return b, site.MessagesPath, true
	}
}

// copyHeaders gives the client's the upstream's headers.
//
// The encoding is left out: the client is told what it gets, not that
// it gets something compressed.
func copyHeaders(w http.ResponseWriter, resp *http.Response) {
	for k, v := range resp.Header {
		switch strings.ToLower(k) {
		case "content-encoding", "content-length", "transfer-encoding":
			continue
		}
		w.Header()[k] = v
	}
}

// writeStream writes the upstream's stream to the client.
//
// The bytes go out as they arrive, so the client gets the first token
// as soon as the upstream sends it. Once the first byte is out the
// account cannot be changed any more; if the stream then breaks, the
// client is told the stream ended early rather than being sent a
// second, different answer.
//
// The event that says so has the shape of the protocol the client
// speaks: an OpenAI client does not read Anthropic's error event, and a
// Codex client reads the Responses one.
func (s *Server) writeStream(w http.ResponseWriter, resp *http.Response, proto protocol, convertBack bool, model string) {
	copyHeaders(w, resp)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var err error
	if convertBack {
		// 转换器自己收尾：它失败的时候已经写了一个 response.failed，
		// 网关再写一个就是第二次失败，形状还不一样。
		err = convert.StreamResponses(resp.Body, w, model)
	} else {
		sniffer := &idSniffer{w: w}
		_, err = io.Copy(sniffer, resp.Body)
		if err != nil {
			writeStreamError(sniffer, proto, err, model, sniffer.responseID())
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	resp.Body.Close()
}

// writeStreamError writes the event that says the stream broke.
//
// The shape is the one of the protocol the client speaks.
func writeStreamError(w io.Writer, proto protocol, err error, model, responseID string) {
	message := "the stream ended early: " + err.Error()
	switch proto {
	case protocolOpenAI:
		// OpenAI 的流里，错误也是一行 data。
		b, _ := json.Marshal(map[string]any{
			"error": map[string]any{"message": message, "type": "api_error"},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
	case protocolCodex:
		// Codex 读的是 Responses 的形状，和本地转换写出来的一样。
		response := map[string]any{
			"object": "response",
			"model":  model,
			"status": "failed",
			"error":  map[string]any{"code": "upstream_error", "message": message},
		}
		if responseID != "" {
			response["id"] = responseID
		}
		b, _ := json.Marshal(map[string]any{
			"type":     "response.failed",
			"response": response,
		})
		fmt.Fprintf(w, "event: response.failed\ndata: %s\n\n", b)
	default:
		b, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": message},
		})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
	}
}

// idSniffer keeps the first bytes of a stream, to find the response id.
//
// The Responses protocol names the response in its first event, and the
// failure at the end of a broken stream has to carry the same id: the
// client matches the two.
type idSniffer struct {
	w   io.Writer
	buf []byte
}

// maxSniff is how many bytes are looked at for the response id.
const maxSniff = 8 << 10

func (s *idSniffer) Write(p []byte) (int, error) {
	if len(s.buf) < maxSniff {
		n := maxSniff - len(s.buf)
		if n > len(p) {
			n = len(p)
		}
		s.buf = append(s.buf, p[:n]...)
	}
	return s.w.Write(p)
}

// responseID gives the id of the response the stream is about.
func (s *idSniffer) responseID() string {
	m := responseIDPattern.FindSubmatch(s.buf)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// responseIDPattern finds the id of the response in the first events.
var responseIDPattern = regexp.MustCompile(`"id"\s*:\s*"(resp[^"]*)"`)

// writeJSON writes a whole answer to the client.
func (s *Server) writeJSON(w http.ResponseWriter, resp *http.Response, body []byte, proto protocol, convertBack bool) {
	if convertBack {
		out, err := convertResponses(body, proto)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		body = out
	}
	copyHeaders(w, resp)
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// convertResponses turns an Anthropic answer into a Responses answer.
func convertResponses(body []byte, proto protocol) ([]byte, error) {
	if proto != protocolCodex {
		return body, nil
	}
	var msg struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("the upstream's answer could not be read")
	}
	text := ""
	for _, c := range msg.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	out := map[string]any{
		"id":     msg.ID,
		"object": "response",
		"status": "completed",
		"output": []any{map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		}},
		"output_text": text,
	}
	return json.Marshal(out)
}

// handleModels serves the model list.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	var access string
	accounts, err := s.Store.ListAccounts(r.Context())
	if err == nil && len(accounts) > 0 {
		if tok, err := s.Mgr.Token(r.Context(), accounts[0].ID); err == nil {
			access = tok.Access
		}
	}
	raw, err := s.Mgr.Up.Models(r.Context(), s.UpstreamBase, access)
	if err != nil || len(raw) == 0 {
		raw = nil
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if raw != nil {
		json.Unmarshal(raw, &out)
	}
	if len(out.Data) == 0 {
		for _, m := range site.FallbackModels() {
			out.Data = append(out.Data, map[string]any{
				"id": m.ID, "object": "model", "owned_by": "minimax",
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   out.Data,
	})
}

// windowReset gives the moment a spent window comes back.
//
// The key is the plan group, not one window: the windows "5 hours" and
// "7 days" belong to a group, and the group comes back as a whole.
func (s *Server) windowReset(ctx context.Context, accountID, groupKey string) time.Time {
	ws, err := s.Mgr.LatestWindows(ctx, accountID)
	if err != nil {
		return time.Time{}
	}
	for _, w := range ws {
		if w.ModelName == groupKey && !w.ResetsAt.IsZero() {
			return w.ResetsAt
		}
	}
	return time.Time{}
}

// backoff is how long to wait before asking again.
//
// The wait grows with the attempt and is short enough that a request
// does not hang for long.
func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * 500 * time.Millisecond
	if d > 4*time.Second {
		d = 4 * time.Second
	}
	return d
}

// clientHeaders gives the headers the client set that are worth
// passing on.
func clientHeaders(r *http.Request) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"anthropic-version", "anthropic-beta", "x-stainless-lang"} {
		if v := r.Header.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// modelOf reads the model out of a request body.
func modelOf(body []byte) string {
	var b struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &b)
	return b.Model
}

// isStream says the request wants a stream.
func isStream(body []byte) bool {
	var b struct {
		Stream bool `json:"stream"`
	}
	json.Unmarshal(body, &b)
	return b.Stream
}

// systemOf reads the system prompt out of a request body.
func systemOf(body []byte) string {
	var b struct {
		System json.RawMessage `json:"system"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	return textOf(b.System)
}

// firstUserOf reads the first user message out of a request body.
//
// The three protocols put it in different places, so all of them are
// looked at.
func firstUserOf(body []byte) string {
	var b struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &b) != nil {
		return ""
	}
	for _, m := range b.Messages {
		if m.Role == "user" {
			return textOf(m.Content)
		}
	}
	if len(b.Input) > 0 {
		var s string
		if json.Unmarshal(b.Input, &s) == nil {
			return s
		}
	}
	return ""
}

// textOf reads the text out of a content field, which may be a string
// or a list of parts.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}
