// Package gateway serves the chat protocols and the management API.
//
// A chat request is bound to an account by its session, and stays
// there. When the account is spent, another is tried and the request
// is replayed — but only while nothing has been written to the client
// yet.
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
	"strings"
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
	NativeResponses bool
	// WebUI serves the management pages, when there is one.
	WebUI http.Handler
}

// New makes a gateway.
func New(mgr *manager.Manager, cfg config.Config, s *store.Store, webui http.Handler) *Server {
	g := &Server{
		Mgr:             mgr,
		Cfg:             cfg,
		Store:           s,
		UpstreamBase:    site.LLMHost,
		NativeResponses: true,
		WebUI:           webui,
	}
	g.Rot = rotator.New(poolAdapter{s}, rotator.Options{
		MaxAttempts: cfg.Rotation.MaxAttempts,
		Cooldown:    cfg.Rotation.ExhaustedCooldown.Duration(),
		WindowGroup: manager.WindowGroup,
		Strategy:    cfg.Rotation.Strategy,
	})
	return g
}

// poolAdapter makes the store into a rotator pool.
type poolAdapter struct{ s *store.Store }

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

func (p poolAdapter) MarkExhausted(ctx context.Context, accountID, windowKey string, until time.Time, reason string) error {
	return p.s.SetExhausted(ctx, accountID, windowKey, until, reason)
}

func (p poolAdapter) Remaining(ctx context.Context, accountID string) (float64, bool) {
	snaps, err := p.s.LatestUsageSnapshots(ctx)
	if err != nil {
		return 0, false
	}
	for _, s := range snaps {
		if s.AccountID != accountID {
			continue
		}
		if !s.HasAmounts {
			return 0, false
		}
		return s.Remaining, true
	}
	return 0, false
}

// ServeHTTP routes a request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.auth(r) {
		writeError(w, http.StatusUnauthorized, "the key was refused")
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

	used := map[string]bool{}
	var lastErr error
	attempts := s.Cfg.Rotation.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	for attempt := 0; attempt < attempts; attempt++ {
		plan, err := s.Rot.Pick(ctx, sess.Key, model, used)
		if err != nil {
			if lastErr != nil {
				writeError(w, http.StatusTooManyRequests, lastErr.Error())
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
			SessionID: sess.UpstreamID,
			// 秒，UTC 以东。参考实现发的是 getTimezoneOffset() * -60，
			// 和 Zone() 给的偏移是同一个值。time.Time 没有 Offset()
			// 方法，Zone() 的第二个返回值就是它。
			TZOffset:  nowOffset(),
			Body:      upBody,
			Stream:    stream,
			Anthropic: upPath == site.MessagesPath,
			Extra:     clientHeaders(r),
		}

		resp, raw, err := s.attempt(ctx, chatReq, client.Transport, stream)
		if err != nil {
			var oe *upstream.OtherSiteError
			if errors.As(err, &oe) {
				// 跨站：不换号、不消耗 refresh token
				writeError(w, http.StatusBadGateway, oe.Error())
				return
			}
			lastErr = err
			time.Sleep(backoff(attempt))
			continue
		}

		// 上游没有 /responses：改用本地转换，同一个账号再来一次
		if resp.StatusCode == http.StatusNotFound && proto == protocolCodex && s.NativeResponses {
			s.NativeResponses = false
			upBody, upPath, convertBack = s.upstreamShape(body, proto)
			chatReq.Body = upBody
			chatReq.Path = upPath
			chatReq.Anthropic = upPath == site.MessagesPath
			resp, raw, err = s.attempt(r.Context(), chatReq, client.Transport, stream)
			if err != nil {
				lastErr = err
				time.Sleep(backoff(attempt))
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

		failure := rotator.Failure{Status: resp.StatusCode, Body: raw}
		switch rotator.Decide(failure) {
		case rotator.ActionFailover:
			until := s.windowReset(ctx, plan.AccountID, plan.WindowKey)
			_ = s.Rot.MarkExhausted(ctx, plan.AccountID, plan.WindowKey, "quota", until)
			lastErr = fmt.Errorf("the account's credits or plan are used up")
			continue
		case rotator.ActionRenew:
			newTok, rerr := s.Mgr.ForceRenew(ctx, plan.AccountID)
			if rerr != nil {
				// 跨站：不换号、不消耗 refresh token
				var oe *upstream.OtherSiteError
				if errors.As(rerr, &oe) {
					writeError(w, http.StatusBadGateway, oe.Error())
					return
				}
				lastErr = rerr
				continue
			}
			// 用新 token 再来一次，还是这个账号
			chatReq.Access = newTok.Access
			resp2, raw2, err2 := s.attempt(r.Context(), chatReq, client.Transport, stream)
			if err2 != nil {
				var oe *upstream.OtherSiteError
				if errors.As(err2, &oe) {
					writeError(w, http.StatusBadGateway, oe.Error())
					return
				}
				lastErr = err2
				continue
			}
			if resp2.StatusCode == http.StatusOK && raw2 == nil {
				s.writeStream(w, resp2, proto, convertBack, model)
				return
			}
			if resp2.StatusCode == http.StatusOK {
				if err := upstream.CheckEnvelope("chat", raw2); err == nil {
					s.writeJSON(w, resp2, raw2, proto, convertBack)
					return
				}
			}
			lastErr = fmt.Errorf("HTTP %d", resp2.StatusCode)
			continue
		case rotator.ActionRetrySame:
			if isQuotaRefusal(resp.StatusCode, raw) {
				writeRateLimit(w, quotaMessage(raw))
				return
			}
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			time.Sleep(backoff(attempt))
			continue
		default:
			if isQuotaRefusal(resp.StatusCode, raw) {
				writeRateLimit(w, quotaMessage(raw))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(raw)
			return
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("the request could not be served")
	}
	writeError(w, http.StatusTooManyRequests, lastErr.Error())
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
		if s.NativeResponses {
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
func (s *Server) writeStream(w http.ResponseWriter, resp *http.Response, proto protocol, convertBack bool, model string) {
	copyHeaders(w, resp)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var err error
	if convertBack {
		err = convert.StreamResponses(resp.Body, w, model)
	} else {
		_, err = io.Copy(w, resp.Body)
	}
	if err != nil {
		// 流到一半断了：给客户端一个错误事件收尾，不重放
		fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"the stream ended early: %v\"}}\n\n", err)
	}
	if flusher != nil {
		flusher.Flush()
	}
	resp.Body.Close()
}

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
func (s *Server) windowReset(ctx context.Context, accountID, windowKey string) time.Time {
	ws, err := s.Mgr.LatestWindows(ctx, accountID)
	if err != nil {
		return time.Time{}
	}
	for _, w := range ws {
		if w.ModelName == windowKey && !w.ResetsAt.IsZero() {
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
