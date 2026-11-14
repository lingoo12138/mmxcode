package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToResponsesKeepsTheToolCall says the tool call of an Anthropic
// answer is a function call item in the Responses answer.
//
// 回答里的工具调用块在 Responses 里有一个自己的样子。把它丢掉，模型
// 就永远调不了工具：客户端收到的是一个说完话了的回答。
func TestToResponsesKeepsTheToolCall(t *testing.T) {
	answer := []byte(`{"type":"message","id":"msg_1","model":"m","content":[
		{"type":"text","text":"ok"},
		{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Beijing"}}
	]}`)
	out, err := ToResponses(answer, "m")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Status    string `json:"status"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		OutputText string `json:"output_text"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "response" || resp.Status != "completed" {
		t.Fatalf("the answer = %s", out)
	}
	if resp.OutputText != "ok" {
		t.Fatalf("output_text = %q", resp.OutputText)
	}
	var call *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Status    string `json:"status"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	for i := range resp.Output {
		if resp.Output[i].Type == "function_call" {
			call = &resp.Output[i]
		}
	}
	if call == nil {
		t.Fatalf("the tool call was thrown away: %s", out)
	}
	if call.CallID != "toolu_1" || call.Name != "get_weather" {
		t.Fatalf("the tool call = %+v", call)
	}
	if call.Arguments != `{"city":"Beijing"}` {
		t.Fatalf("arguments = %q", call.Arguments)
	}
	if call.Status != "completed" {
		t.Fatalf("status = %q", call.Status)
	}
	if call.ID == "" {
		t.Fatal("the tool call has no id of its own")
	}
}

// TestToResponsesHasTheSameItemsAsTheStream says the whole answer and
// the stream agree on what the items look like.
//
// 同一个上游答案，两条路要给客户端看一样的形状。
func TestToResponsesHasTheSameItemsAsTheStream(t *testing.T) {
	answer := []byte(`{"type":"message","id":"msg_1","content":[
		{"type":"text","text":"ok"},
		{"type":"thinking","thinking":"hmm"},
		{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}
	]}`)
	out, err := ToResponses(answer, "m")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Output []struct {
			Type    string `json:"type"`
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, item := range resp.Output {
		kinds = append(kinds, item.Type)
	}
	want := []string{"message", "reasoning", "function_call"}
	if len(kinds) != len(want) {
		t.Fatalf("the items are %v, want %v", kinds, want)
	}
	for i, k := range kinds {
		if k != want[i] {
			t.Fatalf("the items are %v, want %v", kinds, want)
		}
	}
	if len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Type != "output_text" {
		t.Fatalf("the message item = %+v", resp.Output[0])
	}
	if len(resp.Output[1].Summary) != 1 || resp.Output[1].Summary[0].Text != "hmm" {
		t.Fatalf("the reasoning item = %+v", resp.Output[1])
	}
}

// TestToResponsesCarriesTheUsage says the upstream's usage comes along.
func TestToResponsesCarriesTheUsage(t *testing.T) {
	answer := []byte(`{"type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":5}}`)
	out, err := ToResponses(answer, "m")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Usage == nil {
		t.Fatalf("the usage was thrown away: %s", out)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 || resp.Usage.TotalTokens != 15 {
		t.Fatalf("the usage = %+v", resp.Usage)
	}
}

// TestToAnthropicOpenAITurnsAMessage says an OpenAI request becomes an
// Anthropic one.
func TestToAnthropicOpenAITurnsAMessage(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[
			{"role":"system","content":"you are helpful"},
			{"role":"user","content":"hi"}
		],
		"max_completion_tokens":32
	}`)
	out, err := ToAnthropicOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Model     string `json:"model"`
		System    string `json:"system"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "m" {
		t.Fatalf("model = %q", req.Model)
	}
	if req.System != "you are helpful" {
		t.Fatalf("system = %q", req.System)
	}
	if req.MaxTokens != 32 {
		t.Fatalf("max_tokens = %d", req.MaxTokens)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.Messages[0].Role != "user" {
		t.Fatalf("role = %q", req.Messages[0].Role)
	}
	if len(req.Messages[0].Content) != 1 || req.Messages[0].Content[0].Text != "hi" {
		t.Fatalf("content = %+v", req.Messages[0].Content)
	}
}

// TestToAnthropicOpenAITurnsTheTools says the tools and the tool
// choice come across.
func TestToAnthropicOpenAITurnsTheTools(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"f"}}
	}`)
	out, err := ToAnthropicOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "f" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if len(req.Tools[0].InputSchema) == 0 {
		t.Fatalf("the schema was thrown away: %s", out)
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != "tool" || req.ToolChoice.Name != "f" {
		t.Fatalf("tool_choice = %+v", req.ToolChoice)
	}
}

// TestToAnthropicOpenAITurnsTheToolCalls says the tool calls of a
// conversation come across as tool_use blocks.
func TestToAnthropicOpenAITurnsTheToolCalls(t *testing.T) {
	in := []byte(`{
		"model":"m",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"42"}
		]
	}`)
	out, err := ToAnthropicOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   string          `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	assistant := req.Messages[1]
	if assistant.Role != "assistant" || len(assistant.Content) != 1 {
		t.Fatalf("the assistant message = %+v", assistant)
	}
	block := assistant.Content[0]
	if block.Type != "tool_use" || block.ID != "call_1" || block.Name != "f" {
		t.Fatalf("the tool_use block = %+v", block)
	}
	if string(block.Input) != `{"a":1}` {
		t.Fatalf("the tool's input = %s", block.Input)
	}
	result := req.Messages[2]
	if result.Role != "user" || len(result.Content) != 1 {
		t.Fatalf("the tool result message = %+v", result)
	}
	if result.Content[0].Type != "tool_result" || result.Content[0].ToolUseID != "call_1" {
		t.Fatalf("the tool_result block = %+v", result.Content[0])
	}
	if result.Content[0].Content != "42" {
		t.Fatalf("the result = %q", result.Content[0].Content)
	}
}

// TestToChatCompletionSaysWhatWasSaid says an Anthropic answer becomes
// an OpenAI chat completion.
func TestToChatCompletionSaysWhatWasSaid(t *testing.T) {
	answer := []byte(`{"type":"message","id":"msg_1","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	out, err := ToChatCompletion(answer, "m")
	if err != nil {
		t.Fatal(err)
	}
	var completion struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &completion); err != nil {
		t.Fatal(err)
	}
	if completion.Object != "chat.completion" {
		t.Fatalf("object = %q", completion.Object)
	}
	if completion.Created == 0 {
		t.Fatal("the completion has no time")
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("choices = %+v", completion.Choices)
	}
	if completion.Choices[0].Message.Content != "hi" {
		t.Fatalf("content = %q", completion.Choices[0].Message.Content)
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", completion.Choices[0].FinishReason)
	}
	if completion.Usage.PromptTokens != 3 || completion.Usage.CompletionTokens != 2 || completion.Usage.TotalTokens != 5 {
		t.Fatalf("the usage = %+v", completion.Usage)
	}
}

// TestToChatCompletionTurnsTheToolCall says the tool call of an
// Anthropic answer is an OpenAI tool call.
func TestToChatCompletionTurnsTheToolCall(t *testing.T) {
	answer := []byte(`{"type":"message","id":"msg_1","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}]}`)
	out, err := ToChatCompletion(answer, "m")
	if err != nil {
		t.Fatal(err)
	}
	var completion struct {
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
	if err := json.Unmarshal(out, &completion); err != nil {
		t.Fatal(err)
	}
	calls := completion.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %+v", calls)
	}
	if calls[0].ID != "toolu_1" || calls[0].Type != "function" {
		t.Fatalf("the tool call = %+v", calls[0])
	}
	if calls[0].Function.Name != "f" {
		t.Fatalf("name = %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("arguments = %q", calls[0].Function.Arguments)
	}
	if completion.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", completion.Choices[0].FinishReason)
	}
}

// TestStreamChatCompletionsGivesTheChunks says the Anthropic stream
// becomes an OpenAI stream.
func TestStreamChatCompletionsGivesTheChunks(t *testing.T) {
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
	if !strings.HasSuffix(strings.TrimSpace(stream), "data: [DONE]") {
		t.Fatalf("the stream does not end with [DONE]: %s", stream)
	}
	var text strings.Builder
	var finish string
	chunks := 0
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
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("a chunk is not JSON: %v (%s)", err, payload)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("object = %q", chunk.Object)
		}
		chunks++
		text.WriteString(chunk.Choices[0].Delta.Content)
		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
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

// TestStreamChatCompletionsGivesTheToolCall says the tool call of a
// stream is an OpenAI tool call.
func TestStreamChatCompletionsGivesTheToolCall(t *testing.T) {
	up := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
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
	var name, args, id string
	var finish string
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
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("a chunk is not JSON: %v (%s)", err, payload)
		}
		for _, call := range chunk.Choices[0].Delta.ToolCalls {
			if call.Function.Name != "" {
				name = call.Function.Name
			}
			if call.ID != "" {
				id = call.ID
			}
			args += call.Function.Arguments
		}
		if chunk.Choices[0].FinishReason != nil {
			finish = *chunk.Choices[0].FinishReason
		}
	}
	if name != "f" {
		t.Fatalf("the tool's name = %q", name)
	}
	if id != "toolu_1" {
		t.Fatalf("the tool call's id = %q", id)
	}
	if args != `{"a":1}` {
		t.Fatalf("the arguments = %q", args)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q", finish)
	}
}
