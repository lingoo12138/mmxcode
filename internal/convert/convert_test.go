package convert

import (
	"encoding/json"
	"fmt"
	"reflect"
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

// TestAnEmptyTextBlockStillHasItsText says a text block keeps its
// text field even when the text is empty.
//
// Anthropic wants the field: a block that says only "type":"text" is
// refused. And a block that carries a field it does not know is
// refused too, so the text block carries nothing else.
func TestAnEmptyTextBlockStillHasItsText(t *testing.T) {
	out, err := ToAnthropic([]byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	block := m["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if _, ok := block["text"]; !ok {
		t.Fatalf("block = %v, want a text field", block)
	}
	if block["text"] != "" {
		t.Fatalf("block = %v, want an empty text", block)
	}
	if len(block) != 2 {
		t.Fatalf("block = %v, want only the type and the text", block)
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

// TestStreamEmpty: an empty stream is a stream that ended before the
// answer was done. The client has to be able to tell that from a
// whole answer, so the conversion fails and no response.completed is
// written.
func TestStreamEmpty(t *testing.T) {
	var out strings.Builder
	err := StreamResponses(strings.NewReader(""), &out, "m")
	if err == nil {
		t.Fatal("expected an error: the stream ended early")
	}
	got := out.String()
	if !strings.Contains(got, "response.created") {
		t.Fatalf("the stream has no created event:\n%s", got)
	}
	if strings.Contains(got, "response.completed") {
		t.Fatalf("the stream says it completed:\n%s", got)
	}
}

// events reads the events out of a Responses stream.
func events(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(s, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m); err != nil {
				t.Fatalf("the event's data is not JSON: %v", err)
			}
			out = append(out, m)
		}
	}
	return out
}

// eventTypes gives the type of every event of a stream.
func eventTypes(s string) []string {
	var out []string
	for _, block := range strings.Split(s, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				var m map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) == nil {
					out = append(out, fmt.Sprint(m["type"]))
				}
			}
		}
	}
	return out
}

// Finding 1: an upstream error must not be swallowed. The client gets
// a failure event and the conversion gives an error back, so that the
// gateway's own handling of a stream that ended early fires too.
func TestStreamUpstreamErrorFails(t *testing.T) {
	cases := []struct {
		name string
		sse  string
	}{
		{
			name: "an error event",
			sse: "event: error\n" +
				`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n",
		},
		{
			name: "an error field",
			sse: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1}}}` + "\n\n" +
				"event: error\n" +
				`data: {"type":"error","error":{"message":"the account is not usable"}}` + "\n\n",
		},
		{
			name: "a statusInfo code",
			sse: "event: message_start\n" +
				`data: {"type":"message_start","statusInfo":{"code":1004,"message":"account error"}}` + "\n\n",
		},
		{
			name: "a base_resp code",
			sse: "event: message_start\n" +
				`data: {"type":"message_start","base_resp":{"status_code":1002,"status_msg":"bad request"}}` + "\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			err := StreamResponses(strings.NewReader(tc.sse), &out, "m")
			if err == nil {
				t.Fatal("expected an error: the upstream said the answer failed")
			}
			got := out.String()
			if strings.Contains(got, "response.completed") {
				t.Fatalf("the stream says it completed:\n%s", got)
			}
			if !strings.Contains(got, "response.failed") {
				t.Fatalf("the stream has no failure event:\n%s", got)
			}
			// The message of the upstream has to reach the client.
			for _, ev := range events(t, got) {
				if ev["type"] != "response.failed" {
					continue
				}
				resp := ev["response"].(map[string]any)
				errObj, ok := resp["error"].(map[string]any)
				if !ok {
					t.Fatalf("the failure has no error: %v", resp)
				}
				if msg := fmt.Sprint(errObj["message"]); msg == "" {
					t.Fatalf("the failure has no message: %v", errObj)
				}
			}
		})
	}
}

// Finding 2: a stream that ends early is not a whole answer.
func TestStreamTruncatedIsNotComplete(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"half an ans"}}` + "\n\n"

	var out strings.Builder
	err := StreamResponses(strings.NewReader(in), &out, "m")
	if err == nil {
		t.Fatal("expected an error: the stream ended before the answer was done")
	}
	got := out.String()
	if strings.Contains(got, "response.completed") {
		t.Fatalf("the stream says it completed:\n%s", got)
	}
	if !strings.Contains(got, "response.failed") {
		t.Fatalf("the stream has no failure event:\n%s", got)
	}
	// The text that did arrive is still there.
	if !strings.Contains(got, `"delta":"half an ans"`) {
		t.Fatalf("the text is missing:\n%s", got)
	}
}

// Finding 3: the response id belongs to the Responses protocol, is
// stable inside one response and is not the upstream's message id.
func TestStreamResponseID(t *testing.T) {
	run := func() string {
		var out strings.Builder
		if err := StreamResponses(strings.NewReader(anthropicSSE), &out, "m"); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	first := run()
	second := run()

	idOf := func(s string) string {
		for _, ev := range events(t, s) {
			if ev["type"] == "response.created" {
				return fmt.Sprint(ev["response"].(map[string]any)["id"])
			}
		}
		t.Fatalf("the stream has no created event:\n%s", s)
		return ""
	}
	id, other := idOf(first), idOf(second)
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("the id is not a Responses id: %q", id)
	}
	if id == other {
		t.Fatalf("two responses have the same id: %q", id)
	}
	// The upstream's message id must not be used.
	if strings.Contains(id, "msg_") {
		t.Fatalf("the id is the upstream's message id: %q", id)
	}
	// One response keeps one id.
	for _, ev := range events(t, first) {
		resp, ok := ev["response"].(map[string]any)
		if !ok {
			continue
		}
		if got := fmt.Sprint(resp["id"]); got != id {
			t.Fatalf("the events of one response have different ids: %q and %q", id, got)
		}
	}
}

// Finding 4: a block the Responses protocol cannot show does not
// become an empty message, and it leaves no item without an end.
func TestStreamSkipsUnknownContentBlock(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"image","source":{"type":"base64","data":"xx"}}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var out strings.Builder
	if err := StreamResponses(strings.NewReader(in), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	// Every item that was added is also done.
	added := map[string]bool{}
	done := map[string]bool{}
	for _, ev := range events(t, got) {
		switch ev["type"] {
		case "response.output_item.added":
			added[fmt.Sprint(ev["item"].(map[string]any)["id"])] = true
		case "response.output_item.done":
			done[fmt.Sprint(ev["item"].(map[string]any)["id"])] = true
		}
	}
	if len(added) == 0 {
		t.Fatalf("the stream has no item:\n%s", got)
	}
	for id := range added {
		if !done[id] {
			t.Fatalf("the item %s was added but never done:\n%s", id, got)
		}
	}
	// The picture did not become an empty message.
	for _, ev := range events(t, got) {
		if ev["type"] != "response.output_text.done" {
			continue
		}
		if fmt.Sprint(ev["text"]) == "" {
			t.Fatalf("an empty text was written:\n%s", got)
		}
	}
	// The completed answer has only the message.
	for _, ev := range events(t, got) {
		if ev["type"] != "response.completed" {
			continue
		}
		out := ev["response"].(map[string]any)["output"].([]any)
		if len(out) != 1 {
			t.Fatalf("the answer has %d items: %v", len(out), out)
		}
		if fmt.Sprint(out[0].(map[string]any)["type"]) != "message" {
			t.Fatalf("the item is not a message: %v", out[0])
		}
	}
}

// Finding 5: the tool choice comes across.
func TestToAnthropicToolChoice(t *testing.T) {
	cases := []struct {
		name string
		give string
		want string // the Anthropic tool_choice, "" for none
	}{
		{name: "auto", give: `"auto"`, want: `{"type":"auto"}`},
		{name: "none", give: `"none"`, want: `{"type":"none"}`},
		{name: "required becomes any", give: `"required"`, want: `{"type":"any"}`},
		{name: "a named function", give: `{"type":"function","name":"get_weather"}`, want: `{"type":"tool","name":"get_weather"}`},
		{name: "a name that is not known", give: `"maybe"`, want: ""},
		{name: "a function without a name", give: `{"type":"function"}`, want: ""},
		{name: "a shape that is not known", give: `{"type":"allowed_tools","mode":"auto"}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(`{"model":"m","input":"hi","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],"tool_choice":` + tc.give + `}`)
			out, err := ToAnthropic(in)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			got, ok := m["tool_choice"]
			if tc.want == "" {
				if ok {
					t.Fatalf("the tool choice should have been left out: %v", got)
				}
				return
			}
			if !ok {
				t.Fatalf("the tool choice is missing: %s", out)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			// The names of the fields are not what matters here, only
			// what they say.
			var have, want map[string]any
			if err := json.Unmarshal(b, &have); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(have, want) {
				t.Fatalf("tool_choice = %s, want %s", b, tc.want)
			}
		})
	}
}

// Finding 6, request side: the arguments of a tool call have to be a
// JSON object on the Anthropic side.
func TestToAnthropicToolArguments(t *testing.T) {
	cases := []struct {
		name    string
		give    string
		want    string
		wantErr bool
	}{
		{name: "a JSON object as a string", give: `"{\"city\":\"SF\"}"`, want: `{"city":"SF"}`},
		{name: "an object", give: `{"city":"SF"}`, want: `{"city":"SF"}`},
		{name: "no arguments", give: `""`, want: `{}`},
		{name: "text that is not JSON", give: `"SF"`, wantErr: true},
		{name: "JSON that is not an object", give: `"[1,2]"`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := []byte(`{"model":"m","input":[{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":` + tc.give + `}]}`)
			out, err := ToAnthropic(in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %s", out)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			msgs := m["messages"].([]any)
			block := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
			input, ok := block["input"].(map[string]any)
			if !ok {
				t.Fatalf("the tool input is not an object: %v", block)
			}
			b, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Fatalf("input = %s, want %s", b, tc.want)
			}
		})
	}
}

// Finding 6, stream side: the arguments of the finished tool call are
// one JSON string.
func TestStreamFunctionCallArgumentsAreOneString(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":":\"SF\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var out strings.Builder
	if err := StreamResponses(strings.NewReader(in), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	var args string
	for _, ev := range events(t, got) {
		if ev["type"] != "response.output_item.done" {
			continue
		}
		item := ev["item"].(map[string]any)
		if fmt.Sprint(item["type"]) != "function_call" {
			continue
		}
		s, ok := item["arguments"].(string)
		if !ok {
			t.Fatalf("the arguments are not a string: %v", item)
		}
		args = s
	}
	if args == "" {
		t.Fatalf("the finished tool call has no arguments:\n%s", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("the arguments are not one JSON string: %q: %v", args, err)
	}
	if parsed["city"] != "SF" {
		t.Fatalf("the arguments are wrong: %q", args)
	}
}

// Finding 7: the completed answer carries the items that were sent and
// uses the names of the Responses protocol.
func TestStreamCompletedPayload(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(anthropicSSE), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, ev := range events(t, got) {
		if ev["type"] != "response.completed" {
			continue
		}
		resp := ev["response"].(map[string]any)
		if _, ok := resp["output"].([]any); !ok {
			t.Fatalf("the completed answer has no output: %v", resp)
		}
		items := resp["output"].([]any)
		if len(items) != 2 {
			t.Fatalf("the answer has %d items: %v", len(items), items)
		}
		if fmt.Sprint(items[0].(map[string]any)["type"]) != "reasoning" {
			t.Fatalf("the first item is not a reasoning: %v", items[0])
		}
		if fmt.Sprint(items[1].(map[string]any)["type"]) != "message" {
			t.Fatalf("the second item is not a message: %v", items[1])
		}
		if resp["output_text"] != "Hello" {
			t.Fatalf("output_text = %v", resp["output_text"])
		}
		// The names of the Anthropic protocol have no place here.
		for _, key := range []string{"stop_reason", "content", "role"} {
			if _, ok := resp[key]; ok {
				t.Fatalf("the completed answer has the Anthropic field %q: %v", key, resp)
			}
		}
		return
	}
	t.Fatalf("the stream has no completed event:\n%s", got)
}

// Finding 8: every event says where it stands in the stream.
func TestStreamSequenceNumbers(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(anthropicSSE), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for i, ev := range events(t, got) {
		seq, ok := ev["sequence_number"].(float64)
		if !ok {
			t.Fatalf("the event %d has no sequence_number: %v", i, ev)
		}
		if int(seq) != i {
			t.Fatalf("the event %d has the sequence number %v", i, seq)
		}
	}
}

// Finding 9: a data field may be continued over several lines.
func TestStreamMultiLineData(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{
			// The shape the SSE specification asks for: every line
			// carries the data field, and the lines belong to one
			// payload.
			name: "the data field is written again",
			in: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,` + "\n" +
				`data: "content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,` + "\n" +
				`data: "delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
				"event: content_block_stop\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		},
		{
			// The shape some upstreams send: only the first line of
			// the payload carries the data field.
			name: "the payload is spread over the lines",
			in: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
				"event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,` + "\n" +
				`"content_block":{"type":"text","text":""}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,` + "\n" +
				`"delta":{"type":"text_delta","text":"Hello"}}` + "\n\n" +
				"event: content_block_stop\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			if err := StreamResponses(strings.NewReader(tc.in), &out, "m"); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if !strings.Contains(got, `"delta":"Hello"`) {
				t.Fatalf("the text is missing:\n%s", got)
			}
			if !strings.Contains(got, "response.completed") {
				t.Fatalf("the stream did not finish:\n%s", got)
			}
		})
	}
}

// Finding 9: a piece of the stream that cannot be read must not leave
// the client with a false "completed".
func TestStreamMalformedPayloadIsNotComplete(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var out strings.Builder
	err := StreamResponses(strings.NewReader(in), &out, "m")
	if err == nil {
		t.Fatal("expected an error: a piece of the stream could not be read")
	}
	got := out.String()
	if strings.Contains(got, "response.completed") {
		t.Fatalf("the stream says it completed:\n%s", got)
	}
	if !strings.Contains(got, "response.failed") {
		t.Fatalf("the stream has no failure event:\n%s", got)
	}
}

// Finding 10: a message with no text in it is not an empty answer.
func TestStreamEmptyTextBlockIsNotAMessage(t *testing.T) {
	in := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var out strings.Builder
	if err := StreamResponses(strings.NewReader(in), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, ev := range events(t, got) {
		switch ev["type"] {
		case "response.output_text.done", "response.content_part.done":
			if fmt.Sprint(ev["text"]) == "" {
				t.Fatalf("an empty text was written:\n%s", got)
			}
		case "response.output_item.added", "response.output_item.done":
			item := ev["item"].(map[string]any)
			if fmt.Sprint(item["type"]) != "message" {
				continue
			}
			content, ok := item["content"].([]any)
			if !ok {
				continue
			}
			for _, part := range content {
				if fmt.Sprint(part.(map[string]any)["text"]) == "" {
					t.Fatalf("an empty message was written:\n%s", got)
				}
			}
		}
	}
	// The answer has no item at all: the model said nothing.
	for _, ev := range events(t, got) {
		if ev["type"] != "response.completed" {
			continue
		}
		items := ev["response"].(map[string]any)["output"].([]any)
		if len(items) != 0 {
			t.Fatalf("the answer has %d items: %v", len(items), items)
		}
	}
}

// The events of a stream keep their order.
func TestStreamEventOrder(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(anthropicSSE), &out, "m"); err != nil {
		t.Fatal(err)
	}
	got := eventTypes(out.String())
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(got) != len(want) {
		t.Fatalf("the events are %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the events are %v, want %v", got, want)
		}
	}
}
