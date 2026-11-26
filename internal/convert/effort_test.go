package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// upstreamThinking reads the thinking settings out of a request we made.
//
// 它读的是发给上游的那个请求体：thinking.type 说开不开，
// output_config.effort 说多深。
func upstreamThinking(t *testing.T, body []byte) (typ, effort string, hasOutput bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("the request is not JSON: %v (%s)", err, body)
	}
	if th, ok := m["thinking"].(map[string]any); ok {
		typ, _ = th["type"].(string)
	}
	if oc, ok := m["output_config"].(map[string]any); ok {
		hasOutput = true
		effort, _ = oc["effort"].(string)
	}
	return typ, effort, hasOutput
}

// TestTheOpenAIEffortReachesTheUpstream says the depth the client asks
// for reaches the model.
//
// OpenAI 那边的话是 reasoning_effort。上游（MiniMax 的 Anthropic 路）
// 说的是另一套：thinking.type 说开不开，output_config.effort 说多深。
// 不搬过去的话，客户端要「浅一点」，模型还是按最深的来 —— 更多的字、
// 更慢、更贵，而客户端不知道。
func TestTheOpenAIEffortReachesTheUpstream(t *testing.T) {
	out, err := ToAnthropicOpenAI([]byte(`{"model":"m","reasoning_effort":"low",` +
		`"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	typ, effort, hasOutput := upstreamThinking(t, out)
	if typ != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive", typ)
	}
	if !hasOutput {
		t.Fatalf("the request has no output_config: %s", out)
	}
	if effort != "low" {
		t.Fatalf("output_config.effort = %q, want low", effort)
	}
}

// TestTheResponsesEffortReachesTheUpstream says the same for the Codex
// door.
//
// 这一门以前读了 reasoning.effort，但只留下「adaptive 思考」，
// 深度那一档丢了。
func TestTheResponsesEffortReachesTheUpstream(t *testing.T) {
	out, err := ToAnthropic([]byte(`{"model":"m","reasoning":{"effort":"high"},"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	typ, effort, hasOutput := upstreamThinking(t, out)
	if typ != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive", typ)
	}
	if !hasOutput {
		t.Fatalf("the request has no output_config: %s", out)
	}
	if effort != "high" {
		t.Fatalf("output_config.effort = %q, want high", effort)
	}
}

// TestMinimalEffortBecomesLow says the one word MiniMax does not know
// becomes the nearest one it does.
//
// minimal 是 OpenAI 那边的说法。MiniMax 的枚举里没有它（只有 low、
// medium、high、xhigh、max），但它比 low 还浅 —— 落到 low 上。
func TestMinimalEffortBecomesLow(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func() ([]byte, error)
	}{
		{"chat", func() ([]byte, error) {
			return ToAnthropicOpenAI([]byte(`{"model":"m","reasoning_effort":"minimal",` +
				`"messages":[{"role":"user","content":"hi"}]}`))
		}},
		{"responses", func() ([]byte, error) {
			return ToAnthropic([]byte(`{"model":"m","reasoning":{"effort":"minimal"},"input":"hi"}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tc.make()
			if err != nil {
				t.Fatal(err)
			}
			_, effort, hasOutput := upstreamThinking(t, out)
			if !hasOutput {
				t.Fatalf("the request has no output_config: %s", out)
			}
			if effort != "low" {
				t.Fatalf("output_config.effort = %q, want low", effort)
			}
		})
	}
}

// TestNoneEffortTurnsTheThinkingOff says "no reasoning" is turned into
// "no thinking".
//
// none 不是一档深度，是把思考关掉。M3 真的就不了；M3.1-Flash 总是想，
// 上游会自己答 400 —— 跟它自己的 OpenAI/Responses 端点一个脾气。
func TestNoneEffortTurnsTheThinkingOff(t *testing.T) {
	out, err := ToAnthropic([]byte(`{"model":"m","reasoning":{"effort":"none"},"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	typ, _, hasOutput := upstreamThinking(t, out)
	if typ != "disabled" {
		t.Fatalf("thinking.type = %q, want disabled", typ)
	}
	if hasOutput {
		t.Fatalf("the request has an output_config: %s", out)
	}
}

// TestNoEffortSaysNothing says a request that says nothing about
// thinking gets nothing.
//
// M3 的思考是关着的（客户端没说就是不说），M3.1-Flash 总是想。这两件
// 都由上游定，网关不替它决定。
func TestNoEffortSaysNothing(t *testing.T) {
	out, err := ToAnthropic([]byte(`{"model":"m","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	typ, _, hasOutput := upstreamThinking(t, out)
	if typ != "" {
		t.Fatalf("thinking.type = %q, want none", typ)
	}
	if hasOutput {
		t.Fatalf("the request has an output_config: %s", out)
	}
}

// TestTheThinkingTextComesBackInReasoningContent says the OpenAI door
// carries the thinking.
//
// MiniMax 自己的 OpenAI 端点把思考放在 reasoning_content 里（那个端点
// 默认就是分开的，除非客户端说 reasoning_split=false）。我们这一门以前
// 把 thinking 块整个扔掉：客户端只看见答案，思考的字不见了。
func TestTheThinkingTextComesBackInReasoningContent(t *testing.T) {
	out, err := ToChatCompletion([]byte(`{"type":"message","id":"msg_1","model":"m","stop_reason":"end_turn",`+
		`"content":[{"type":"thinking","thinking":"let me think"},{"type":"text","text":"hi"}]}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, out)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %+v", got.Choices)
	}
	if got.Choices[0].Message.ReasoningContent != "let me think" {
		t.Fatalf("reasoning_content = %q", got.Choices[0].Message.ReasoningContent)
	}
	if got.Choices[0].Message.Content != "hi" {
		t.Fatalf("content = %q", got.Choices[0].Message.Content)
	}
}

// TestTheThinkingTextComesBackInTheStream says the same for the stream.
//
// 思考是一段一段来的，每一段都进 reasoning_content —— 客户端看着它
// 一步一步长出来，跟看答案一样。
func TestTheThinkingTextComesBackInTheStream(t *testing.T) {
	up := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":5}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
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

	var reasoning, content strings.Builder
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("a chunk is not JSON: %v (%s)", err, payload)
		}
		for _, c := range chunk.Choices {
			reasoning.WriteString(c.Delta.ReasoningContent)
			content.WriteString(c.Delta.Content)
		}
	}
	if reasoning.String() != "let me think" {
		t.Fatalf("reasoning_content = %q, want %q", reasoning.String(), "let me think")
	}
	if content.String() != "hi" {
		t.Fatalf("content = %q, want %q", content.String(), "hi")
	}
}
