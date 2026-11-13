// Package convert turns the Codex Responses protocol into what
// MiniMax speaks, and the answer back.
//
// MiniMax Code speaks the Anthropic Messages protocol natively. The
// Responses protocol is turned into it here: the request is rewritten,
// and the upstream's stream is rewritten back into Responses events.
package convert

import (
	"encoding/json"
	"fmt"
)

// responsesRequest is the part of a Responses request that is read.
type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Stream          bool            `json:"stream"`
	Tools           []responsesTool `json:"tools"`
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
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream,omitempty"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Thinking  *thinking          `json:"thinking,omitempty"`
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

// anthropicTool is one tool of an Anthropic request.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// thinking is the Anthropic thinking setting.
type thinking struct {
	Type string `json:"type"`
}

// ToAnthropic turns a Responses request into an Anthropic Messages
// request.
//
// The model, the messages, the tools and the streaming flag all come
// across. The Responses protocol's tool calls and tool results become
// Anthropic's tool_use and tool_result blocks.
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
	if in.Reasoning != nil && in.Reasoning.Effort != "" {
		out.Thinking = &thinking{Type: "adaptive"}
	}
	msgs, err := messages(in.Input)
	if err != nil {
		return nil, err
	}
	out.Messages = msgs
	return json.Marshal(out)
}

// messages turns the Responses input into Anthropic messages.
func messages(raw json.RawMessage) ([]anthropicMessage, error) {
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
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("responses: input is neither a string nor a list")
	}
	var out []anthropicMessage
	for _, item := range items {
		typ := stringField(item["type"])
		switch typ {
		case "function_call":
			var fc struct {
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			if err := json.Unmarshal(mustMarshal(item), &fc); err != nil {
				return nil, err
			}
			input := json.RawMessage(fc.Arguments)
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
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
			if err := json.Unmarshal(mustMarshal(item), &fo); err != nil {
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
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal(mustMarshal(item), &mi); err != nil {
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

// blocks turns a Responses message's content into Anthropic blocks.
func blocks(raw json.RawMessage) ([]anthropicBlock, error) {
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

func stringField(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
