package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamChatCompletionsCarriesTheUsage says the tokens of a stream
// reach the client.
//
// 非流式的那一趟说得很清楚：这趟用了多少。非流式之外的这一趟以前一个字
// 都不说 —— 客户端拿到的是一段没有用处的回答，账单和配额都对不上。
//
// 用量放在最后一块里（choices 是空的），这是 OpenAI 自己那个样子：
// include_usage 的时候，它就是这么发的。
func TestStreamChatCompletionsCarriesTheUsage(t *testing.T) {
	up := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":5}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
	var out strings.Builder
	if err := StreamChatCompletions(strings.NewReader(up), &out, "m"); err != nil {
		t.Fatal(err)
	}
	stream := out.String()

	var usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	}
	found := false
	usageAt := -1
	chunkAt := 0
	for i, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" {
			continue
		}
		chunkAt++
		var chunk struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("a chunk is not JSON: %v (%s)", err, payload)
		}
		if chunk.Usage == nil {
			continue
		}
		if len(chunk.Choices) != 0 {
			t.Fatalf("the usage chunk has %d choices: %s", len(chunk.Choices), payload)
		}
		usage = struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		}{chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens, chunk.Usage.TotalTokens}
		found = true
		usageAt = i
	}
	if !found {
		t.Fatalf("the stream does not say what it used: %s", stream)
	}
	if usage.PromptTokens != 5 || usage.CompletionTokens != 3 || usage.TotalTokens != 8 {
		t.Fatalf("usage = %+v", usage)
	}
	if usageAt == 0 {
		t.Fatalf("the usage is in the first chunk: %s", stream)
	}
	if chunkAt < 2 {
		t.Fatalf("the stream has %d chunks: %s", chunkAt, stream)
	}
}
