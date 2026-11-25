package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// cachedSSE is a sample of the upstream's stream when the prompt hit the
// cache.
//
// 这是上游自己文档里的那个例子。开头那一块说整个提示词有多长（1366），
// 末尾那一块才说清哪一部分是新的（1252）、哪一部分命中了缓存（114）。
// 末尾说的算数。
const cachedSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":1366}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":1252,"output_tokens":213,"cache_creation_input_tokens":0,"cache_read_input_tokens":114}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// cachedAnswer is the upstream's whole answer when the prompt hit the
// cache: 13 个新字进去，159 个字是从缓存里读出来的。
const cachedAnswer = `{"type":"message","id":"msg_1","model":"m","stop_reason":"end_turn",` +
	`"content":[{"type":"text","text":"hi"}],` +
	`"usage":{"input_tokens":13,"output_tokens":149,"cache_creation_input_tokens":0,"cache_read_input_tokens":159}}`

// TestToChatCompletionCarriesTheCachedTokens says the cached tokens of a
// whole answer reach the client of the OpenAI door.
//
// 上游只说「13 个新字进去，159 个字命中了缓存」。OpenAI 那边要的是一个
// 总数（prompt_tokens）和一个命中数（cached_tokens）——总数里含着命中
// 的那部分。以前只把 13 报上去，提示词的大小就少了一大半：客户端算出
// 来的「没命中」是 13，「命中」是 0。
func TestToChatCompletionCarriesTheCachedTokens(t *testing.T) {
	out, err := ToChatCompletion([]byte(cachedAnswer), "m")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			Details          struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, out)
	}
	// prompt_tokens 是总数：13 + 159
	if got.Usage.PromptTokens != 172 {
		t.Fatalf("prompt_tokens = %d, want 172", got.Usage.PromptTokens)
	}
	if got.Usage.CompletionTokens != 149 {
		t.Fatalf("completion_tokens = %d, want 149", got.Usage.CompletionTokens)
	}
	if got.Usage.TotalTokens != 321 {
		t.Fatalf("total_tokens = %d, want 321", got.Usage.TotalTokens)
	}
	if got.Usage.Details.CachedTokens != 159 {
		t.Fatalf("cached_tokens = %d, want 159", got.Usage.Details.CachedTokens)
	}
}

// TestToResponsesCarriesTheCachedTokens says the same for the Codex
// door.
//
// Responses 那边的形状不一样：总数叫 input_tokens，命中数在
// input_tokens_details 里。数还是那些数。
func TestToResponsesCarriesTheCachedTokens(t *testing.T) {
	out, err := ToResponses([]byte(cachedAnswer), "m")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
			Details      struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, out)
	}
	if got.Usage.InputTokens != 172 {
		t.Fatalf("input_tokens = %d, want 172", got.Usage.InputTokens)
	}
	if got.Usage.OutputTokens != 149 {
		t.Fatalf("output_tokens = %d, want 149", got.Usage.OutputTokens)
	}
	if got.Usage.TotalTokens != 321 {
		t.Fatalf("total_tokens = %d, want 321", got.Usage.TotalTokens)
	}
	if got.Usage.Details.CachedTokens != 159 {
		t.Fatalf("cached_tokens = %d, want 159", got.Usage.Details.CachedTokens)
	}
}

// TestStreamChatCompletionsCarriesTheCachedTokens says the cached tokens
// of a stream reach the client of the OpenAI door.
//
// 流里的数是分两处说的：开头一块说整个提示词有多长，末尾一块才说清
// 哪一部分是新的、哪一部分命中了缓存。末尾说的算数。
func TestStreamChatCompletionsCarriesTheCachedTokens(t *testing.T) {
	var out strings.Builder
	if err := StreamChatCompletions(strings.NewReader(cachedSSE), &out, "m"); err != nil {
		t.Fatal(err)
	}
	stream := out.String()

	var usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
		Details          struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	found := false
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
				Details          struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("a chunk is not JSON: %v (%s)", err, payload)
		}
		if chunk.Usage == nil {
			continue
		}
		usage = struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			Details          struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		}{chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens, chunk.Usage.Details}
		found = true
	}
	if !found {
		t.Fatalf("the stream does not say what it used: %s", stream)
	}
	// 1252 个新的 + 114 个命中的 = 1366
	if usage.PromptTokens != 1366 {
		t.Fatalf("prompt_tokens = %d, want 1366", usage.PromptTokens)
	}
	if usage.CompletionTokens != 213 {
		t.Fatalf("completion_tokens = %d, want 213", usage.CompletionTokens)
	}
	if usage.TotalTokens != 1579 {
		t.Fatalf("total_tokens = %d, want 1579", usage.TotalTokens)
	}
	if usage.Details.CachedTokens != 114 {
		t.Fatalf("cached_tokens = %d, want 114", usage.Details.CachedTokens)
	}
}

// TestStreamResponsesCarriesTheCachedTokens says the same for the Codex
// door's stream.
//
// 这一门以前还有第二个毛病：它只认开头那一块里的 input_tokens。MiniMax
// 的流开头说的是 0（那时候还不知道），末尾才说 1252 —— 客户端拿到的
// 输入字数是 0。
func TestStreamResponsesCarriesTheCachedTokens(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(cachedSSE), &out, "m"); err != nil {
		t.Fatal(err)
	}
	stream := out.String()

	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
		Details      struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	found := false
	for _, ev := range events(t, stream) {
		if ev["type"] != "response.completed" {
			continue
		}
		resp, _ := ev["response"].(map[string]any)
		if resp == nil {
			continue
		}
		raw, ok := resp["usage"]
		if !ok {
			continue
		}
		b, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &usage); err != nil {
			t.Fatalf("the usage is not JSON: %v (%s)", err, b)
		}
		found = true
	}
	if !found {
		t.Fatalf("the stream does not say what it used: %s", stream)
	}
	if usage.InputTokens != 1366 {
		t.Fatalf("input_tokens = %d, want 1366", usage.InputTokens)
	}
	if usage.OutputTokens != 213 {
		t.Fatalf("output_tokens = %d, want 213", usage.OutputTokens)
	}
	if usage.TotalTokens != 1579 {
		t.Fatalf("total_tokens = %d, want 1579", usage.TotalTokens)
	}
	if usage.Details.CachedTokens != 114 {
		t.Fatalf("cached_tokens = %d, want 114", usage.Details.CachedTokens)
	}
}
