package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/lingoo12138/mmxcode/internal/site"
)

// upstreamRecorder keeps what the upstream was asked.
//
// 记下发给上游的东西：上游是在自己的 goroutine 里 answering 的，所以
// 要一把锁。
type upstreamRecorder struct {
	mu     sync.Mutex
	paths  []string
	bodies [][]byte
}

func (u *upstreamRecorder) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.paths = append(u.paths, r.URL.Path)
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
}

// seen gives the paths and the bodies the upstream was asked with.
func (u *upstreamRecorder) seen() ([]string, [][]byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...), append([][]byte(nil), u.bodies...)
}

// TestTheOpenAIAnswerComesBackInTheOpenAIShape says an OpenAI request
// that the site cannot answer itself is answered in the OpenAI shape.
//
// 参考实现只走 Anthropic 的 /messages，站点多半没有 /chat/completions。
// 那条路是本地转的：请求转成 Anthropic 的，回答再转回 OpenAI 的样子。
// 客户端认的是 OpenAI 的形状，不能给它一段 Anthropic 的。
func TestTheOpenAIAnswerComesBackInTheOpenAIShape(t *testing.T) {
	rec := &upstreamRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.URL.Path == site.ChatCompletionsPath {
			// 站点没有这条路
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"no such endpoint"}}`))
			return
		}
		w.Write([]byte(`{"type":"message","id":"msg_1","content":[{"type":"text","text":"hi"}]}`))
	})

	body := `{"model":"m","max_completion_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}

	// 上游收到的是 Anthropic 的请求
	paths, bodies := rec.seen()
	if len(paths) != 2 {
		t.Fatalf("the upstream was asked %d times: %v", len(paths), paths)
	}
	if paths[0] != site.ChatCompletionsPath {
		t.Fatalf("the first path = %q, want the native one", paths[0])
	}
	if paths[1] != site.MessagesPath {
		t.Fatalf("the second path = %q, want the Anthropic one", paths[1])
	}
	var up map[string]any
	if err := json.Unmarshal(bodies[1], &up); err != nil {
		t.Fatalf("the upstream's request is not JSON: %v (%s)", err, bodies[1])
	}
	if up["max_tokens"] != float64(32) {
		t.Fatalf("the upstream's request = %s", bodies[1])
	}

	// 客户端收到的是 OpenAI 的回答
	var answer struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if answer.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", answer.Object)
	}
	if len(answer.Choices) != 1 {
		t.Fatalf("choices = %+v", answer.Choices)
	}
	if answer.Choices[0].Message.Role != "assistant" {
		t.Fatalf("role = %q", answer.Choices[0].Message.Role)
	}
	if answer.Choices[0].Message.Content != "hi" {
		t.Fatalf("content = %q", answer.Choices[0].Message.Content)
	}
	if answer.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", answer.Choices[0].FinishReason)
	}
	// 第一次 404 把这条路关掉了，后面的请求不再去撞它。
	if h.gw.NativeChatCompletions.Load() {
		t.Fatal("the native path is still on")
	}
}

// TestTheOpenAIToolCallSurvivesTheFallback says a tool call the
// upstream made comes back as an OpenAI tool call.
//
// 模型要调工具，客户端得看得见它调的是哪个工具、拿什么参数去调。
func TestTheOpenAIToolCallSurvivesTheFallback(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == site.ChatCompletionsPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"type":"message","id":"msg_1","stop_reason":"tool_use","content":[` +
			`{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Beijing"}}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var answer struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if len(answer.Choices) != 1 {
		t.Fatalf("choices = %+v", answer.Choices)
	}
	calls := answer.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %+v", calls)
	}
	if calls[0].ID != "toolu_1" {
		t.Fatalf("id = %q, want the tool_use block's id", calls[0].ID)
	}
	if calls[0].Type != "function" {
		t.Fatalf("type = %q", calls[0].Type)
	}
	if calls[0].Function.Name != "get_weather" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"city":"Beijing"}` {
		t.Fatalf("arguments = %q", calls[0].Function.Arguments)
	}
	if answer.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", answer.Choices[0].FinishReason)
	}
}

// TestTheOpenAIRequestIsUntouchedWhenTheSiteHasThePath says the
// native path is the default: nothing is converted when the site
// serves /chat/completions.
//
// 站点也许真的有这条路。有的时候网关就该原样把请求发过去：转来转去
// 只会把东西弄丢。
func TestTheOpenAIRequestIsUntouchedWhenTheSiteHasThePath(t *testing.T) {
	rec := &upstreamRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[` +
			`{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	})

	body := `{"model":"m","max_completion_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	paths, bodies := rec.seen()
	if len(paths) != 1 || paths[0] != site.ChatCompletionsPath {
		t.Fatalf("the upstream was asked %v", paths)
	}
	if string(bodies[0]) != body {
		t.Fatalf("the upstream's request = %s, want it unchanged", bodies[0])
	}
	// 客户端拿到的就是上游的那一段
	if !strings.Contains(w.Body.String(), "chatcmpl-1") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if !h.gw.NativeChatCompletions.Load() {
		t.Fatal("the native path was turned off without a 404")
	}
}

// TestTheOpenAIStreamFallbackEndsWithDone says the streaming fallback
// gives the client an OpenAI stream.
//
// OpenAI 的流是一段 data，每一段是一块，最后是 [DONE]。少了最后那一
// 段，客户端会一直等一个不会来的答案。
func TestTheOpenAIStreamFallbackEndsWithDone(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == site.ChatCompletionsPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":5}}}\n\n"))
		w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
		w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
		w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"))
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	})

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	stream := w.Body.String()
	if !strings.HasSuffix(strings.TrimSpace(stream), "data: [DONE]") {
		t.Fatalf("the stream does not end with [DONE]: %s", stream)
	}

	var text strings.Builder
	var finish string
	chunks := 0
	usage := struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}{}
	sawUsage := false
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("a chunk is not JSON: %v (%s)", err, payload)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("object = %q, want chat.completion.chunk", chunk.Object)
		}
		if chunk.Model != "m" {
			t.Fatalf("model = %q", chunk.Model)
		}
		if chunk.Usage != nil {
			// 最后那块只有用量：choices 是空的（OpenAI 自己就那个
			// 样子）。
			sawUsage = true
			usage.PromptTokens = chunk.Usage.PromptTokens
			usage.CompletionTokens = chunk.Usage.CompletionTokens
			usage.TotalTokens = chunk.Usage.TotalTokens
			if len(chunk.Choices) != 0 {
				t.Fatalf("the usage chunk has choices: %+v", chunk.Choices)
			}
			continue
		}
		chunks++
		if len(chunk.Choices) != 1 {
			t.Fatalf("choices = %+v", chunk.Choices)
		}
		text.WriteString(chunk.Choices[0].Delta.Content)
		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
	}
	if !sawUsage {
		t.Fatalf("the stream does not say what it used: %s", stream)
	}
	if usage.PromptTokens != 5 || usage.CompletionTokens != 3 || usage.TotalTokens != 8 {
		t.Fatalf("usage = %+v", usage)
	}
	if chunks < 2 {
		t.Fatalf("the stream has %d chunks: %s", chunks, stream)
	}
	if text.String() != "hi" {
		t.Fatalf("the text = %q", text.String())
	}
	if finish != "stop" {
		t.Fatalf("finish_reason = %q", finish)
	}
}

// TestTheOpenAIRequestFallsBackWhenTheOpenPlatformIsNotConfigured says a
// 503 from the open platform is "this account has no route there", not a
// failure to hand to the client.
//
// MiniMax 的开放平台是给 API key 用的：MiniMax Code 的账号在那条路上没有
// 配好的路由，站点答的是 503 和 direct_route_not_configured。这不是站点
// 坏了，是这条路对这个账号不通 —— 跟"没有这条路"是一回事，该走 Anthropic
// 那条。
func TestTheOpenAIRequestFallsBackWhenTheOpenPlatformIsNotConfigured(t *testing.T) {
	rec := &upstreamRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.URL.Path == site.ChatCompletionsPath {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"open platform service unavailable, please retry",` +
				`"errorCode":50115,"errorReason":"direct_route_not_configured"}`))
			return
		}
		w.Write([]byte(`{"type":"message","id":"msg_1","content":[{"type":"text","text":"hi"}]}`))
	})

	body := `{"model":"m","max_completion_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	paths, _ := rec.seen()
	if len(paths) != 2 {
		t.Fatalf("the upstream was asked %d times: %v", len(paths), paths)
	}
	if paths[1] != site.MessagesPath {
		t.Fatalf("the second path = %q, want the Anthropic one", paths[1])
	}
	// 这一趟把这个账号的这条路关掉了，后面的请求不再去撞它。
	if h.gw.NativeChatCompletions.Load() {
		t.Fatal("the native path is still on")
	}
}

// TestTheCodexRequestFallsBackWhenTheOpenPlatformIsNotConfigured says the
// same for the Responses protocol.
//
// Codex 客户端走的是 /v1/responses，那条路在开放平台上也不通。
func TestTheCodexRequestFallsBackWhenTheOpenPlatformIsNotConfigured(t *testing.T) {
	rec := &upstreamRecorder{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		if r.URL.Path == site.ResponsesPath {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"open platform service unavailable, please retry",` +
				`"errorCode":50115,"errorReason":"direct_route_not_configured"}`))
			return
		}
		w.Write([]byte(`{"type":"message","id":"msg_1","content":[{"type":"text","text":"hi"}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	paths, _ := rec.seen()
	if len(paths) != 2 {
		t.Fatalf("the upstream was asked %d times: %v", len(paths), paths)
	}
	if paths[1] != site.MessagesPath {
		t.Fatalf("the second path = %q, want the Anthropic one", paths[1])
	}
	if h.gw.NativeResponses.Load() {
		t.Fatal("the native path is still on")
	}
}
