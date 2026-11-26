// Package convert turns the Codex Responses protocol into what
// MiniMax speaks, and the answer back.
//
// MiniMax Code speaks the Anthropic Messages protocol natively. The
// Responses protocol is turned into it here: the request is rewritten,
// and the upstream's stream is rewritten back into Responses events.
//
// Two rules hold for the whole package:
//
//   - A piece of the answer that the Responses protocol has no shape
//     for is left out, not faked. An Anthropic image block, a document
//     or a tool result has no output item of its own in the Responses
//     protocol, so it gets no item at all. Turning it into an empty
//     message would tell the client that the model said nothing,
//     which is not what happened.
//
//   - A tool call's arguments have to be a JSON object on the
//     Anthropic side and a JSON string on the Responses side.
//     Arguments that are neither are refused rather than turned into
//     an empty object: an empty object is a tool call with arguments
//     the caller never wrote.
package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// responsesRequest is the part of a Responses request that is read.
type responsesRequest struct {
	Model           string          `json:"model"`
	Input           rawJSON         `json:"input"`
	Instructions    string          `json:"instructions"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Stream          bool            `json:"stream"`
	Tools           []responsesTool `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Reasoning       *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

// responsesTool is one tool of a Responses request.
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// anthropicRequest is the Anthropic Messages request that comes out.
type anthropicRequest struct {
	Model      string               `json:"model"`
	System     string               `json:"system,omitempty"`
	Messages   []anthropicMessage   `json:"messages"`
	MaxTokens  int                  `json:"max_tokens"`
	Stream     bool                 `json:"stream,omitempty"`
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
	Thinking   *thinking            `json:"thinking,omitempty"`
	// OutputConfig says how deep the model should think. MiniMax 的
	// Anthropic 路用 output_config.effort 说这件事。
	OutputConfig *outputConfig `json:"output_config,omitempty"`
	// StopSequences are the sequences that end the answer. The
	// Responses protocol has no such field, so it is only filled in
	// when the request came from the OpenAI protocol.
	StopSequences []string `json:"stop_sequences,omitempty"`
}

// anthropicMessage is one message.
type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is one block of a message.
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

// MarshalJSON writes the block the way Anthropic wants it.
//
// Anthropic wants the field every block has, even when the field is
// empty: a text block without a "text" is refused. And a block that
// carries a field it does not know is refused too, so each kind writes
// only its own fields — a text block never carries an id.
func (b anthropicBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "tool_use":
		input := b.Input
		if len(input) == 0 {
			// Anthropic wants an object here, not nothing.
			input = json.RawMessage("{}")
		}
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{b.Type, b.ID, b.Name, input})
	case "tool_result":
		return json.Marshal(struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
		}{b.Type, b.ToolUseID, b.Content})
	default:
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{b.Type, b.Text})
	}
}

// anthropicTool is one tool of an Anthropic request.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicToolChoice is Anthropic's tool_choice.
//
// Anthropic always wants an object, even for the choices that the
// Responses protocol writes as a plain string.
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// thinking is the Anthropic thinking setting.
type thinking struct {
	Type string `json:"type"`
}

// ToAnthropic turns a Responses request into an Anthropic Messages
// request.
//
// The model, the messages, the tools, the tool choice and the
// streaming flag all come across. The Responses protocol's tool calls
// and tool results become Anthropic's tool_use and tool_result blocks.
func ToAnthropic(body []byte) ([]byte, error) {
	var in responsesRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("responses: %w", err)
	}
	out := anthropicRequest{
		Model:     in.Model,
		System:    in.Instructions,
		MaxTokens: in.MaxOutputTokens,
		Stream:    in.Stream,
	}
	if out.MaxTokens == 0 {
		out.MaxTokens = 4096
	}
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	out.ToolChoice = toolChoice(in.ToolChoice)
	if in.Reasoning != nil {
		// 深度要搬过去：客户端要「浅一点」，模型就得真的浅一点。
		out.Thinking, out.OutputConfig = thinkingFor("", in.Reasoning.Effort)
	}
	msgs, err := messages(in.Input)
	if err != nil {
		return nil, err
	}
	out.Messages = msgs
	return marshalTo(out, sizeOf(out))
}

// toolChoice turns the Responses tool_choice into Anthropic's.
//
// The Responses protocol says "required" where Anthropic says "any",
// and it names a function where Anthropic names a tool. A choice that
// has no counterpart is left out: a different choice would silently
// change what the model is allowed to do.
func toolChoice(raw json.RawMessage) *anthropicToolChoice {
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
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	if obj.Type == "function" && obj.Name != "" {
		return &anthropicToolChoice{Type: "tool", Name: obj.Name}
	}
	return nil
}

// messages turns the Responses input into Anthropic messages.
func messages(raw rawJSON) ([]anthropicMessage, error) {
	if len(raw) == 0 {
		return []anthropicMessage{}, nil
	}
	// 一个字符串就是一条 user 消息
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []anthropicMessage{{
			Role:    "user",
			Content: []anthropicBlock{{Type: "text", Text: s}},
		}}, nil
	}
	var items []rawJSON
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("responses: input is neither a string nor a list")
	}
	var out []anthropicMessage
	for _, item := range items {
		// 每一样东西都是身体里原来那一段字节：抄一份是几十兆的白抄。
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(item, &head); err != nil {
			continue
		}
		switch head.Type {
		case "function_call":
			var fc struct {
				CallID    string          `json:"call_id"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(item, &fc); err != nil {
				return nil, err
			}
			input, err := toolInput(fc.Arguments)
			if err != nil {
				return nil, err
			}
			out = append(out, anthropicMessage{
				Role: "assistant",
				Content: []anthropicBlock{{
					Type: "tool_use", ID: fc.CallID, Name: fc.Name, Input: input,
				}},
			})
		case "function_call_output":
			var fo struct {
				CallID string `json:"call_id"`
				Output string `json:"output"`
			}
			if err := json.Unmarshal(item, &fo); err != nil {
				return nil, err
			}
			out = append(out, anthropicMessage{
				Role: "user",
				Content: []anthropicBlock{{
					Type: "tool_result", ToolUseID: fo.CallID, Content: fo.Output,
				}},
			})
		case "message", "":
			var mi struct {
				Role    string  `json:"role"`
				Content rawJSON `json:"content"`
			}
			if err := json.Unmarshal(item, &mi); err != nil {
				return nil, err
			}
			role := mi.Role
			if role == "" {
				role = "user"
			}
			if role == "system" {
				role = "user"
			}
			blocks, err := blocks(mi.Content)
			if err != nil {
				return nil, err
			}
			out = append(out, anthropicMessage{Role: role, Content: blocks})
		default:
			// 别的类型不认，跳过
		}
	}
	if len(out) == 0 {
		out = []anthropicMessage{{
			Role:    "user",
			Content: []anthropicBlock{{Type: "text", Text: ""}},
		}}
	}
	return out, nil
}

// toolInput turns the arguments of a Responses tool call into the
// input of an Anthropic tool_use.
//
// Anthropic wants an object and the Responses protocol carries the
// arguments as a JSON string. Arguments that are not a JSON object
// cannot be used, so they are refused: an empty object would be a
// tool call with arguments the caller never wrote.
func toolInput(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			// 没有参数的工具就是这样写的。
			return json.RawMessage(`{}`), nil
		}
		if _, err := jsonObject([]byte(s)); err != nil {
			return nil, err
		}
		return json.RawMessage(s), nil
	}
	if _, err := jsonObject(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// jsonObject says whether a piece of JSON is an object.
func jsonObject(raw []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("responses: the tool call's arguments are not a JSON object")
	}
	if obj == nil {
		// "null" reads as an object with nothing in it, but it is
		// not one.
		return nil, fmt.Errorf("responses: the tool call's arguments are not a JSON object")
	}
	return obj, nil
}

// blocks turns a Responses message's content into Anthropic blocks.
func blocks(raw rawJSON) ([]anthropicBlock, error) {
	if len(raw) == 0 {
		return []anthropicBlock{{Type: "text", Text: ""}}, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []anthropicBlock{{Type: "text", Text: s}}, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("responses: content is neither a string nor a list")
	}
	out := make([]anthropicBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text", "":
			out = append(out, anthropicBlock{Type: "text", Text: p.Text})
		default:
			// 图片之类的原样丢掉：Anthropic 的图片块形状不同，
			// 这里只保证纯文本的对话能用。
			out = append(out, anthropicBlock{Type: "text", Text: p.Text})
		}
	}
	if len(out) == 0 {
		out = []anthropicBlock{{Type: "text", Text: ""}}
	}
	return out, nil
}
