package convert

import (
	"encoding/json"
	"fmt"
	"strings"
)

// anthropicAnswer is the shape of an Anthropic message.
//
// It is the answer of the Anthropic protocol, whole or in pieces: the
// same fields are in the message of a whole answer and in the
// content_block of a stream.
type anthropicAnswer struct {
	// ID is the message's id.
	ID string `json:"id"`
	// Model is the model that answered.
	Model string `json:"model"`
	// StopReason says why the answer ended.
	StopReason string `json:"stop_reason"`
	// Content is the answer's blocks.
	Content []anthropicContent `json:"content"`
	// Usage says how many tokens were used.
	Usage *anthropicUsage `json:"usage"`
}

// anthropicContent is one block of an answer.
type anthropicContent struct {
	// Type is the block's kind: text, thinking, tool_use.
	Type string `json:"type"`
	// Text is the text of a text block.
	Text string `json:"text"`
	// Thinking is the text of a thinking block.
	Thinking string `json:"thinking"`
	// ID is the id of a tool_use block.
	ID string `json:"id"`
	// Name is the tool's name, for a tool_use block.
	Name string `json:"name"`
	// Input is the tool's arguments, for a tool_use block.
	Input json.RawMessage `json:"input"`
}

// anthropicUsage is what an answer says about its tokens.
type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ToResponses turns an Anthropic answer into a Responses answer.
//
// The items are the ones the streaming converter writes for the very
// same answer: a text block is a message item, a thinking block is a
// reasoning item, a tool_use block is a function call item. The two
// paths have to agree — an answer must not look different depending
// on whether the client asked for a stream.
//
// A block the Responses protocol has no shape for is left out, not
// faked: it gets no item at all.
func ToResponses(body []byte, model string) ([]byte, error) {
	var msg anthropicAnswer
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("responses: the upstream's answer could not be read")
	}
	if model == "" {
		model = msg.Model
	}
	items := []any{}
	var text, reasoning strings.Builder
	for _, c := range msg.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
			items = append(items, map[string]any{
				"type": "message", "id": randomID("msg"),
				"role": "assistant", "status": "completed",
				"content": []any{map[string]any{
					"type": "output_text", "text": c.Text, "annotations": []any{},
				}},
			})
		case "thinking":
			reasoning.WriteString(c.Thinking)
			items = append(items, map[string]any{
				"type": "reasoning", "id": randomID("rs"),
				"summary": []any{map[string]any{
					"type": "summary_text", "text": c.Thinking,
				}},
			})
		case "tool_use":
			items = append(items, map[string]any{
				"type": "function_call", "id": randomID("fc"),
				"call_id": c.ID, "name": c.Name,
				"arguments": toolArguments(c.Input),
				"status":    "completed",
			})
		}
	}
	out := map[string]any{
		"id":     randomID("resp"),
		"object": "response",
		"status": "completed",
		"model":  model,
		"output": items,
		// output_text 是给不读 output 的客户端准备的：它要的只是那段话。
		"output_text": text.String(),
	}
	if reasoning.Len() > 0 {
		out["reasoning_summary_text"] = reasoning.String()
	}
	if msg.Usage != nil {
		out["usage"] = map[string]any{
			"input_tokens":  msg.Usage.InputTokens,
			"output_tokens": msg.Usage.OutputTokens,
			"total_tokens":  msg.Usage.InputTokens + msg.Usage.OutputTokens,
		}
	}
	return json.Marshal(out)
}

// finishReason says what an Anthropic stop reason means to a client
// that only knows the OpenAI wording.
func finishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}
