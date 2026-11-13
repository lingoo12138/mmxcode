package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToAnthropicPlainMessage(t *testing.T) {
	in := []byte(`{
		"model":"MiniMax-M3",
		"instructions":"you are helpful",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"max_output_tokens":100
	}`)
	out, err := ToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "MiniMax-M3" {
		t.Fatalf("model = %v", m["model"])
	}
	if m["system"] != "you are helpful" {
		t.Fatalf("system = %v", m["system"])
	}
	if m["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens = %v", m["max_tokens"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	msg := msgs[0].(map[string]any)
	if msg["role"] != "user" {
		t.Fatalf("role = %v", msg["role"])
	}
	content := msg["content"].([]any)
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hi" {
		t.Fatalf("block = %v", block)
	}
}

func TestToAnthropicStringInput(t *testing.T) {
	out, err := ToAnthropic([]byte(`{"model":"m","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	msg := msgs[0].(map[string]any)
	block := msg["content"].([]any)[0].(map[string]any)
	if block["text"] != "hello" {
		t.Fatalf("block = %v", block)
	}
}

func TestToAnthropicToolCalls(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"sunny"}
		],
		"tools":[{"type":"function","name":"get_weather","description":"the weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]
	}`)
	out, err := ToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	// 第二条是 assistant 的 tool_use
	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("role = %v", assistant["role"])
	}
	block := assistant["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "get_weather" {
		t.Fatalf("block = %v", block)
	}
	if block["id"] != "call_1" {
		t.Fatalf("id = %v", block["id"])
	}
	// 第三条是 user 的 tool_result
	user := msgs[2].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("role = %v", user["role"])
	}
	rblock := user["content"].([]any)[0].(map[string]any)
	if rblock["type"] != "tool_result" || rblock["tool_use_id"] != "call_1" {
		t.Fatalf("block = %v", rblock)
	}
	if rblock["content"] != "sunny" {
		t.Fatalf("content = %v", rblock["content"])
	}
	// 工具的形状换了
	tools := m["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Fatalf("tool = %v", tool)
	}
	if tool["input_schema"] == nil {
		t.Fatalf("tool has no input_schema: %v", tool)
	}
	if _, ok := tool["parameters"]; ok {
		t.Fatalf("the tool kept its Responses shape: %v", tool)
	}
}

func TestToAnthropicKeepsStream(t *testing.T) {
	out, err := ToAnthropic([]byte(`{"model":"m","input":"x","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Fatalf("out = %s", out)
	}
}

func TestToAnthropicRejectsBadJSON(t *testing.T) {
	if _, err := ToAnthropic([]byte(`not json`)); err == nil {
		t.Fatal("expected an error")
	}
}

// anthropicSSE is a fixed sample of the upstream's stream.
const anthropicSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","model":"MiniMax-M3","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

func TestStreamProducesResponsesEvents(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(anthropicSSE), &out, "MiniMax-M3"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"response.created",
		"response.output_item.added",
		"response.reasoning_summary_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.completed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the stream has no %s:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `"delta":"Hello"`) {
		t.Fatalf("the text delta is missing:\n%s", got)
	}
	if !strings.Contains(got, `"delta":"let me think"`) {
		t.Fatalf("the thinking delta is missing:\n%s", got)
	}
}

func TestStreamProducesFunctionCallEvents(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\""}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var out strings.Builder
	if err := StreamResponses(strings.NewReader(in), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		`"type":"function_call"`,
		`"name":"get_weather"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the stream has no %s:\n%s", want, got)
		}
	}
}

func TestStreamEmpty(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(""), &out, "m"); err != nil {
		t.Fatal(err)
	}
	if out.String() == "" {
		t.Fatal("expected at least the created event")
	}
}
