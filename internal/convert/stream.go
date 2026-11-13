package convert

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// anthropicEvent is one event of the upstream's stream.
type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// blockState is what is known about one upstream content block.
type blockState struct {
	// outputIndex is the item's index in the Responses output.
	outputIndex int
	// kind is "reasoning", "message" or "function_call".
	kind string
	// itemID is the Responses item id.
	itemID string
	// callID is the tool call id, for a function call.
	callID string
	// name is the tool's name, for a function call.
	name string
	// text is what has been said so far.
	text string
	// args is the tool arguments so far.
	args string
}

// StreamResponses turns the upstream's Anthropic stream into a
// Responses stream.
//
// The events are written as they come, so a client that is waiting for
// the first token gets it as soon as the upstream sends it.
func StreamResponses(r io.Reader, w io.Writer, model string) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	blocks := map[int]*blockState{}
	nextIndex := 0
	responseID := "resp_" + model
	var text strings.Builder
	var reasoning strings.Builder
	stopReason := ""
	inputTokens, outputTokens := 0, 0

	emit := func(event string, payload map[string]any) error {
		payload["type"] = event
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(bw, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		return bw.Flush()
	}

	// 第一个事件：response.created
	if err := emit("response.created", map[string]any{
		"response": map[string]any{
			"id": responseID, "object": "response", "model": model,
			"status": "in_progress", "output": []any{},
		},
	}); err != nil {
		return err
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			// 认不出的行跳过，不让一行坏数据掐断整条流
			continue
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				responseID = ev.Message.ID
				inputTokens = ev.Message.Usage.InputTokens
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				continue
			}
			st := &blockState{outputIndex: nextIndex}
			nextIndex++
			switch ev.ContentBlock.Type {
			case "thinking":
				st.kind = "reasoning"
				st.itemID = fmt.Sprintf("rs_%d", st.outputIndex)
				if err := emit("response.output_item.added", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "reasoning", "id": st.itemID, "summary": []any{},
					},
				}); err != nil {
					return err
				}
				if err := emit("response.reasoning_summary_part.added", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"summary_index": 0,
					"part":          map[string]any{"type": "summary_text", "text": ""},
				}); err != nil {
					return err
				}
			case "tool_use":
				st.kind = "function_call"
				st.itemID = "fc_" + fmt.Sprint(st.outputIndex)
				st.callID = ev.ContentBlock.ID
				st.name = ev.ContentBlock.Name
				if err := emit("response.output_item.added", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "function_call", "id": st.itemID,
						"call_id": st.callID, "name": st.name, "arguments": "",
					},
				}); err != nil {
					return err
				}
			default:
				st.kind = "message"
				st.itemID = "msg_" + fmt.Sprint(st.outputIndex)
				if err := emit("response.output_item.added", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "message", "id": st.itemID, "role": "assistant",
						"status": "in_progress", "content": []any{},
					},
				}); err != nil {
					return err
				}
				if err := emit("response.content_part.added", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"content_index": 0,
					"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				}); err != nil {
					return err
				}
			}
			blocks[ev.Index] = st
		case "content_block_delta":
			st, ok := blocks[ev.Index]
			if !ok || ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				st.text += ev.Delta.Text
				text.WriteString(ev.Delta.Text)
				if err := emit("response.output_text.delta", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"content_index": 0, "delta": ev.Delta.Text,
				}); err != nil {
					return err
				}
			case "thinking_delta":
				st.text += ev.Delta.Thinking
				reasoning.WriteString(ev.Delta.Thinking)
				if err := emit("response.reasoning_summary_text.delta", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"summary_index": 0, "delta": ev.Delta.Thinking,
				}); err != nil {
					return err
				}
			case "input_json_delta":
				st.args += ev.Delta.PartialJSON
				if err := emit("response.function_call_arguments.delta", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"delta": ev.Delta.PartialJSON,
				}); err != nil {
					return err
				}
			}
		case "content_block_stop":
			st, ok := blocks[ev.Index]
			if !ok {
				continue
			}
			switch st.kind {
			case "message":
				if err := emit("response.output_text.done", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"content_index": 0, "text": st.text,
				}); err != nil {
					return err
				}
				if err := emit("response.content_part.done", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"content_index": 0,
					"part":          map[string]any{"type": "output_text", "text": st.text, "annotations": []any{}},
				}); err != nil {
					return err
				}
				if err := emit("response.output_item.done", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "message", "id": st.itemID, "role": "assistant",
						"status": "completed",
						"content": []any{map[string]any{
							"type": "output_text", "text": st.text, "annotations": []any{},
						}},
					},
				}); err != nil {
					return err
				}
			case "function_call":
				if err := emit("response.function_call_arguments.done", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"arguments": st.args,
				}); err != nil {
					return err
				}
				if err := emit("response.output_item.done", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "function_call", "id": st.itemID,
						"call_id": st.callID, "name": st.name, "arguments": st.args,
					},
				}); err != nil {
					return err
				}
			case "reasoning":
				if err := emit("response.reasoning_summary_part.done", map[string]any{
					"item_id": st.itemID, "output_index": st.outputIndex,
					"summary_index": 0,
					"part":          map[string]any{"type": "summary_text", "text": st.text},
				}); err != nil {
					return err
				}
				if err := emit("response.output_item.done", map[string]any{
					"output_index": st.outputIndex,
					"item": map[string]any{
						"type": "reasoning", "id": st.itemID,
						"summary": []any{map[string]any{"type": "summary_text", "text": st.text}},
					},
				}); err != nil {
					return err
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			if ev.Usage != nil {
				outputTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			// 收尾在循环外统一做
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}

	return emit("response.completed", map[string]any{
		"response": map[string]any{
			"id": responseID, "object": "response", "model": model,
			"status": "completed", "stop_reason": stopReason,
			"output_text":            text.String(),
			"reasoning_summary_text": reasoning.String(),
			"usage": map[string]any{
				"input_tokens": inputTokens, "output_tokens": outputTokens,
			},
		},
	})
}
