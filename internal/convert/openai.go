package convert

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// This file is the OpenAI side of the conversion skeleton.
//
// The reference plugin only ever calls the Anthropic endpoint. When the
// site does not serve /chat/completions, an OpenAI request is turned
// into an Anthropic one here, and the Anthropic answer is turned back
// into the OpenAI shape — the same skeleton the Codex path rides on,
// with another shape at each end.

// chatRequest is the part of an OpenAI chat request that is read.
type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Stream              bool            `json:"stream"`
	Tools               []chatTool      `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	Stop                json.RawMessage `json:"stop"`
}

// chatMessage is one message of an OpenAI request.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// chatToolCall is one tool call of an assistant message.
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatTool is one tool of an OpenAI request.
type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ToAnthropicOpenAI turns an OpenAI chat request into an Anthropic
// Messages request.
//
// The model, the messages, the tools, the tool choice and the
// streaming flag all come across. The OpenAI tool calls and tool
// results become Anthropic's tool_use and tool_result blocks.
//
// The system prompt is put in Anthropic's own place: an OpenAI request
// carries it as a message of its own, and Anthropic has a field for
// it.
func ToAnthropicOpenAI(body []byte) ([]byte, error) {
	var in chatRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("chat: %w", err)
	}
	out := anthropicRequest{
		Model:     in.Model,
		Stream:    in.Stream,
		MaxTokens: in.MaxTokens,
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = in.MaxCompletionTokens
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = 4096
	}
	var system []string
	for _, m := range in.Messages {
		switch m.Role {
		case "system", "developer":
			// Anthropic's system prompt is its own field: the same
			// words in a different place.
			if t := chatText(m.Content); t != "" {
				system = append(system, t)
			}
			continue
		case "user":
			out.Messages = append(out.Messages, anthropicMessage{
				Role: "user", Content: chatTextBlocks(m.Content),
			})
		case "assistant":
			blocks := chatBlocks(m.Content)
			for _, tc := range m.ToolCalls {
				input, err := toolInput(json.RawMessage(tc.Function.Arguments))
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, anthropicBlock{
					Type: "tool_use", ID: tc.ID,
					Name: tc.Function.Name, Input: input,
				})
			}
			if len(blocks) == 0 {
				// 一条什么也没有的助手消息在 Anthropic 那边不成立。
				blocks = []anthropicBlock{{Type: "text", Text: ""}}
			}
			out.Messages = append(out.Messages, anthropicMessage{
				Role: "assistant", Content: blocks,
			})
		case "tool", "function":
			// Anthropic puts the result of a tool in a user message.
			out.Messages = append(out.Messages, anthropicMessage{
				Role: "user", Content: []anthropicBlock{{
					Type: "tool_result", ToolUseID: m.ToolCallID,
					Content: chatText(m.Content),
				}},
			})
		default:
			// 别的角色不认，跳过。
		}
	}
	out.System = strings.Join(system, "\n\n")
	if len(out.Messages) == 0 {
		out.Messages = []anthropicMessage{{
			Role: "user", Content: []anthropicBlock{{Type: "text", Text: ""}},
		}}
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	out.ToolChoice = openAIToolChoice(in.ToolChoice)
	out.StopSequences = chatStop(in.Stop)
	return json.Marshal(out)
}

// openAIToolChoice turns the OpenAI tool_choice into Anthropic's.
//
// The same words as the Responses protocol: "required" is Anthropic's
// "any", and a named function is a named tool.
func openAIToolChoice(raw json.RawMessage) *anthropicToolChoice {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return &anthropicToolChoice{Type: "auto"}
		case "none":
			return &anthropicToolChoice{Type: "none"}
		case "required":
			return &anthropicToolChoice{Type: "any"}
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	if obj.Type == "function" && obj.Function.Name != "" {
		return &anthropicToolChoice{Type: "tool", Name: obj.Function.Name}
	}
	return nil
}

// chatStop reads the stop sequences out of an OpenAI request.
//
// The field is a string or a list of strings; both are read.
func chatStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	return list
}

// chatText reads the text out of an OpenAI message's content.
func chatText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// chatBlocks turns an OpenAI message's content into Anthropic blocks.
//
// The parts that carry text come across; the pictures and the files do
// not: their shapes differ, and a picture turned into a piece of text
// would be a lie.
func chatBlocks(raw json.RawMessage) []anthropicBlock {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []anthropicBlock{{Type: "text", Text: s}}
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]anthropicBlock, 0, len(parts))
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			out = append(out, anthropicBlock{Type: "text", Text: p.Text})
		}
	}
	return out
}

// chatTextBlocks is the blocks of a message that has to say something.
//
// Anthropic 的消息不能是空的：一条没有内容的消息不成立，所以这里
// 补一个空的文字块。
func chatTextBlocks(raw json.RawMessage) []anthropicBlock {
	blocks := chatBlocks(raw)
	if len(blocks) == 0 {
		return []anthropicBlock{{Type: "text", Text: ""}}
	}
	return blocks
}

// ToChatCompletion turns an Anthropic answer into an OpenAI chat
// completion.
//
// A tool_use block becomes a tool_calls entry and the reason for the
// end becomes "tool_calls": a client that is told "stop" while the
// answer holds a tool call waits for nothing.
func ToChatCompletion(body []byte, model string) ([]byte, error) {
	var msg anthropicAnswer
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("chat: the upstream's answer could not be read")
	}
	if model == "" {
		model = msg.Model
	}
	id := msg.ID
	if id == "" {
		id = randomID("chatcmpl")
	}
	var content strings.Builder
	var toolCalls []any
	for _, c := range msg.Content {
		switch c.Type {
		case "text":
			content.WriteString(c.Text)
		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id": c.ID, "type": "function",
				"function": map[string]any{
					"name": c.Name, "arguments": toolArguments(c.Input),
				},
			})
		}
	}
	finish := finishReason(msg.StopReason)
	if len(toolCalls) > 0 {
		finish = "tool_calls"
	}
	message := map[string]any{
		"role": "assistant", "content": content.String(),
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finish,
		}},
	}
	if msg.Usage != nil {
		out["usage"] = map[string]any{
			"prompt_tokens":     msg.Usage.InputTokens,
			"completion_tokens": msg.Usage.OutputTokens,
			"total_tokens":      msg.Usage.InputTokens + msg.Usage.OutputTokens,
		}
	}
	return json.Marshal(out)
}

// chatBlock is what is known about one upstream content block.
type chatBlock struct {
	// kind is "text" or "tool".
	kind string
	// index is the tool call's index in the chunk's tool_calls.
	index int
	// id is the tool call's id.
	id string
	// name is the tool's name.
	name string
	// args are the tool's arguments so far.
	args string
	// input is the whole input the upstream gave with the block.
	input json.RawMessage
}

// chatConverter turns the upstream's Anthropic stream into an OpenAI
// chat completion stream.
type chatConverter struct {
	bw    *bufio.Writer
	model string
	// id is the id of this completion. It is made here and not taken
	// from the upstream: two requests must not answer with the same
	// one.
	id string
	// created is when the answer was made.
	created int64

	blocks map[int]*chatBlock
	// nextIndex is the index the next tool call gets.
	nextIndex int
	// toolCalls says whether the answer holds a tool call.
	toolCalls bool

	// stopReason is why the answer ended.
	stopReason string
	// sawStop says the upstream's stream ended the way it should.
	sawStop bool
}

// StreamChatCompletions turns the upstream's Anthropic stream into an
// OpenAI chat completion stream.
//
// The pieces go out as they come, so a client that is waiting for the
// first token gets it as soon as the upstream sends it. The stream
// ends with a chunk that carries the reason for the end and with
// "[DONE]", which is how an OpenAI client knows the answer is whole.
func StreamChatCompletions(r io.Reader, w io.Writer, model string) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	c := &chatConverter{
		bw:      bw,
		model:   model,
		id:      randomID("chatcmpl"),
		created: time.Now().Unix(),
		blocks:  map[int]*chatBlock{},
	}
	return c.run(r)
}

// run reads the upstream's stream and writes the OpenAI stream.
func (c *chatConverter) run(r io.Reader) error {
	// 第一块带上角色：OpenAI 的客户端从这一块开始读。
	if err := c.chunk(map[string]any{"role": "assistant", "content": ""}); err != nil {
		return err
	}
	err := readSSE(r, func(name, payload string) error {
		payload = strings.TrimSpace(payload)
		if payload == "" {
			return nil
		}
		if payload == "[DONE]" {
			// 有些上游用 [DONE] 收尾，它也是一个像样的结尾。
			c.sawStop = true
			return nil
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			if name == "error" {
				return c.fail(payload)
			}
			return nil
		}
		if _, msg, bad := upstreamError(name, &ev); bad {
			return c.fail(msg)
		}
		if ev.Type == "" && name != "" {
			ev.Type = name
		}
		return c.event(&ev)
	})
	if err != nil {
		var ce *chatFailedError
		if errors.As(err, &ce) {
			// 已经报过了
			return err
		}
		return c.fail(err.Error())
	}
	if !c.sawStop {
		// 断了的流不是一个完整的答案：客户端得知道它没有拿到整个
		// 答案，而不是以为答案就这样。
		return c.fail("the upstream's stream ended before the answer was done")
	}
	return c.finish()
}

// event takes one event of the upstream's stream.
func (c *chatConverter) event(ev *anthropicEvent) error {
	switch ev.Type {
	case "message_start":
		if ev.Message != nil && c.model == "" {
			c.model = ev.Message.Model
		}
	case "content_block_start":
		return c.startBlock(ev)
	case "content_block_delta":
		return c.blockDelta(ev)
	case "content_block_stop":
		return c.stopBlock(ev)
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			c.stopReason = ev.Delta.StopReason
		}
	case "message_stop":
		c.sawStop = true
	}
	return nil
}

// startBlock takes the opening of one upstream content block.
func (c *chatConverter) startBlock(ev *anthropicEvent) error {
	if ev.ContentBlock == nil {
		return nil
	}
	switch ev.ContentBlock.Type {
	case "text", "thinking":
		c.blocks[ev.Index] = &chatBlock{kind: "text"}
		return nil
	case "tool_use":
		st := &chatBlock{
			kind: "tool", index: c.nextIndex,
			id: ev.ContentBlock.ID, name: ev.ContentBlock.Name,
			input: ev.ContentBlock.Input,
		}
		c.nextIndex++
		c.toolCalls = true
		c.blocks[ev.Index] = st
		// 一次工具调用本身就是一件事：它的名字和 id 马上就来。
		return c.toolChunk(map[string]any{
			"index": st.index, "id": st.id, "type": "function",
			"function": map[string]any{"name": st.name, "arguments": ""},
		})
	default:
		// 图片、文档之类的块在 OpenAI 的回答里没有形状，跳过。
		c.blocks[ev.Index] = &chatBlock{kind: "skipped"}
		return nil
	}
}

// blockDelta takes one piece of an upstream content block.
func (c *chatConverter) blockDelta(ev *anthropicEvent) error {
	st, ok := c.blocks[ev.Index]
	if !ok || ev.Delta == nil {
		return nil
	}
	switch ev.Delta.Type {
	case "text_delta":
		if st.kind != "text" || ev.Delta.Text == "" {
			return nil
		}
		return c.chunk(map[string]any{"content": ev.Delta.Text})
	case "input_json_delta":
		if st.kind != "tool" {
			return nil
		}
		st.args += ev.Delta.PartialJSON
		return c.toolChunk(map[string]any{
			"index":    st.index,
			"function": map[string]any{"arguments": ev.Delta.PartialJSON},
		})
	}
	return nil
}

// stopBlock takes the end of one upstream content block.
func (c *chatConverter) stopBlock(ev *anthropicEvent) error {
	st, ok := c.blocks[ev.Index]
	if !ok || st.kind != "tool" {
		return nil
	}
	if st.args == "" {
		// 参数是一次给全的，或者根本没有参数。
		st.args = toolArguments(st.input)
		if st.args == "" {
			st.args = "{}"
		}
		return c.toolChunk(map[string]any{
			"index":    st.index,
			"function": map[string]any{"arguments": st.args},
		})
	}
	return nil
}

// finish writes the end of the answer.
func (c *chatConverter) finish() error {
	reason := finishReason(c.stopReason)
	if c.toolCalls {
		reason = "tool_calls"
	}
	if err := c.write(map[string]any{
		"index": 0, "delta": map[string]any{}, "finish_reason": reason,
	}); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.bw, "data: [DONE]\n\n"); err != nil {
		return err
	}
	return c.bw.Flush()
}

// chunk writes one chunk of the OpenAI stream.
func (c *chatConverter) chunk(delta map[string]any) error {
	return c.write(map[string]any{
		"index": 0, "delta": delta, "finish_reason": nil,
	})
}

// toolChunk writes one chunk that carries a piece of a tool call.
func (c *chatConverter) toolChunk(call map[string]any) error {
	return c.write(map[string]any{
		"index":         0,
		"delta":         map[string]any{"tool_calls": []any{call}},
		"finish_reason": nil,
	})
}

// write writes one chunk of the OpenAI stream.
func (c *chatConverter) write(choice map[string]any) error {
	b, err := json.Marshal(map[string]any{
		"id": c.id, "object": "chat.completion.chunk",
		"created": c.created, "model": c.model,
		"choices": []any{choice},
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.bw, "data: %s\n\n", b); err != nil {
		return err
	}
	return c.bw.Flush()
}

// chatFailedError is the error of a stream that could not be finished.
type chatFailedError struct {
	msg string
}

func (e *chatFailedError) Error() string { return e.msg }

// fail writes the failure of the answer.
//
// An OpenAI client reads the failure out of a chunk like any other
// one, so that is where it goes.
func (c *chatConverter) fail(msg string) error {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error"},
	})
	fmt.Fprintf(c.bw, "data: %s\n\n", b)
	fmt.Fprintf(c.bw, "data: [DONE]\n\n")
	c.bw.Flush()
	return &chatFailedError{msg: msg}
}
