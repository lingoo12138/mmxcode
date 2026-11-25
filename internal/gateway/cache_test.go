package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheAnthropicDoorKeepsTheCachedTokens says the Anthropic door
// hands the cached tokens to the client untouched.
//
// 这一门不转：上游发什么，客户端收什么，一个字节都不差。上游说了
// 「13 个新字进去，159 个字命中了缓存」，客户端就该收到这两句话 ——
// @ai-sdk/anthropic 读的就是这两个数。
//
// 另外两门是本地转的，转过去的时候得自己把这两个数带上（见
// TestTheOpenAIAnswerCarriesTheCachedTokens）。
func TestTheAnthropicDoorKeepsTheCachedTokens(t *testing.T) {
	answer := `{"type":"message","id":"msg_1","model":"m","stop_reason":"end_turn",` +
		`"content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":13,"output_tokens":149,"cache_creation_input_tokens":0,"cache_read_input_tokens":159}}`
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(answer))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.gw.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}

	var got struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, w.Body.String())
	}
	if got.Usage.InputTokens != 13 {
		t.Fatalf("input_tokens = %d, want 13", got.Usage.InputTokens)
	}
	if got.Usage.OutputTokens != 149 {
		t.Fatalf("output_tokens = %d, want 149", got.Usage.OutputTokens)
	}
	if got.Usage.CacheReadInputTokens != 159 {
		t.Fatalf("cache_read_input_tokens = %d, want 159", got.Usage.CacheReadInputTokens)
	}
}
