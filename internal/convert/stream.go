package convert

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"
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
	// Error is the error an error event carries.
	Error json.RawMessage `json:"error"`
	// StatusInfo and BaseResp are MiniMax's own ways of saying that
	// the answer failed inside a 200.
	StatusInfo *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	} `json:"statusInfo"`
	BaseResp *struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// blockState is what is known about one upstream content block.
type blockState struct {
	// outputIndex is the item's index in the Responses output.
	outputIndex int
	// kind is "message", "reasoning", "function_call" or "skipped".
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
	// input is the tool input the upstream gave with the block, for
	// the case that it gives the whole arguments at once.
	input json.RawMessage
	// started says whether the item's events have gone out. A block
	// that says nothing gets no item at all: an empty message is
	// worse than no message.
	started bool
	// finished says whether the item's done event has gone out.
	finished bool
}

// converter turns the upstream's stream into a Responses stream.
type converter struct {
	bw    *bufio.Writer
	model string

	// responseID is the id of this response. It is made here and not
	// taken from the upstream: the upstream's message id is not a
	// response id, and two requests must not get the same one.
	responseID string

	// seq counts the events, so that every one of them can say where
	// it stands in the stream.
	seq int

	// blocks is what is known about each upstream content block.
	blocks map[int]*blockState
	// nextIndex is the index the next output item gets.
	nextIndex int
	// items holds the finished output items by their index.
	items map[int]any

	text      strings.Builder
	reasoning strings.Builder

	inputTokens  int
	outputTokens int

	// sawStop says the upstream's stream ended the way it should.
	sawStop bool

	// failed says the answer has already been reported as failed.
	failed bool
}

// StreamResponses turns the upstream's Anthropic stream into a
// Responses stream.
//
// The events are written as they come, so a client that is waiting for
// the first token gets it as soon as the upstream sends it.
//
// A stream that fails, or that ends before the answer is done, gets a
// response.failed and no response.completed: the client has to be able
// to tell a whole answer from a half one.
func StreamResponses(r io.Reader, w io.Writer, model string) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()

	c := &converter{
		bw:         bw,
		model:      model,
		responseID: randomID("resp"),
		blocks:     map[int]*blockState{},
		items:      map[int]any{},
	}
	return c.run(r)
}

// run reads the upstream's stream and writes the Responses stream.
func (c *converter) run(r io.Reader) error {
	if err := c.emit("response.created", map[string]any{
		"response": map[string]any{
			"id": c.responseID, "object": "response", "model": c.model,
			"status": "in_progress", "output": []any{},
		},
	}); err != nil {
		return err
	}

	// broken holds the first piece of the stream that could not be
	// read. The stream goes on, but it does not end as a whole answer
	// any more.
	var broken error
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
				// 上游说了它自己错了，哪怕这一块不是 JSON。
				return c.fail("upstream_error", payload)
			}
			// 认不出的那一块不掐断整条流，但它也不会被当作
			// 什么都没发生：这一条流不再是完整的答案。
			if broken == nil {
				broken = fmt.Errorf("the upstream sent a piece of the stream that could not be read: %w", err)
			}
			return nil
		}
		if code, msg, bad := upstreamError(name, &ev); bad {
			return c.fail(code, msg)
		}
		if ev.Type == "" && name != "" {
			// 有些上游在 event: 那一行写了名字，数据里没有写。
			ev.Type = name
		}
		return c.event(&ev)
	})
	if err != nil {
		var fe *failedError
		if errors.As(err, &fe) {
			// 已经报过了
			return err
		}
		return c.fail("upstream_error", err.Error())
	}
	if broken != nil {
		return c.fail("upstream_error", broken.Error())
	}
	if !c.sawStop {
		return c.fail("upstream_error", "the upstream's stream ended before the answer was done")
	}
	return c.complete()
}

// event takes one event of the upstream's stream.
func (c *converter) event(ev *anthropicEvent) error {
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			c.inputTokens = ev.Message.Usage.InputTokens
		}
	case "content_block_start":
		return c.startBlock(ev)
	case "content_block_delta":
		return c.blockDelta(ev)
	case "content_block_stop":
		return c.stopBlock(ev)
	case "message_delta":
		if ev.Usage != nil {
			c.outputTokens = ev.Usage.OutputTokens
		}
	case "message_stop":
		c.sawStop = true
	}
	return nil
}

// startBlock takes the opening of one upstream content block.
//
// The blocks the Responses protocol cannot show are left out. They get
// no item at all: an empty message in their place would tell the
// client that the model said nothing, which is not what happened.
func (c *converter) startBlock(ev *anthropicEvent) error {
	if ev.ContentBlock == nil {
		return nil
	}
	st := &blockState{input: ev.ContentBlock.Input}
	switch ev.ContentBlock.Type {
	case "text":
		st.kind = "message"
	case "thinking":
		st.kind = "reasoning"
	case "tool_use":
		st.kind = "function_call"
		st.callID = ev.ContentBlock.ID
		st.name = ev.ContentBlock.Name
	default:
		// 图片、文档、工具结果之类的块在 Responses 的输出里没有
		// 对应的形状，跳过：它们不会变成一个空的消息块。
		st.kind = "skipped"
		c.blocks[ev.Index] = st
		return nil
	}
	c.blocks[ev.Index] = st
	switch st.kind {
	case "function_call":
		// 一次工具调用本身就是一件事，不用等它的参数。
		return c.startItem(st)
	case "message":
		// 有些上游把第一段话直接放在块的开头。
		if ev.ContentBlock.Text != "" {
			return c.addText(st, ev.ContentBlock.Text)
		}
	case "reasoning":
		if ev.ContentBlock.Thinking != "" {
			return c.addThinking(st, ev.ContentBlock.Thinking)
		}
	}
	return nil
}

// startItem writes the opening of one output item.
//
// The item is only started when the block actually says something: an
// item with no text in it would be an empty assistant message, and a
// client cannot tell that from a real one.
func (c *converter) startItem(st *blockState) error {
	if st.started || st.kind == "skipped" {
		return nil
	}
	st.started = true
	st.outputIndex = c.nextIndex
	c.nextIndex++
	switch st.kind {
	case "message":
		st.itemID = randomID("msg")
		if err := c.emit("response.output_item.added", map[string]any{
			"output_index": st.outputIndex,
			"item": map[string]any{
				"type": "message", "id": st.itemID, "role": "assistant",
				"status": "in_progress", "content": []any{},
			},
		}); err != nil {
			return err
		}
		return c.emit("response.content_part.added", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	case "reasoning":
		st.itemID = randomID("rs")
		if err := c.emit("response.output_item.added", map[string]any{
			"output_index": st.outputIndex,
			"item": map[string]any{
				"type": "reasoning", "id": st.itemID, "summary": []any{},
			},
		}); err != nil {
			return err
		}
		return c.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": ""},
		})
	case "function_call":
		st.itemID = randomID("fc")
		return c.emit("response.output_item.added", map[string]any{
			"output_index": st.outputIndex,
			"item": map[string]any{
				"type": "function_call", "id": st.itemID,
				"call_id": st.callID, "name": st.name, "arguments": "",
				"status": "in_progress",
			},
		})
	}
	return nil
}

// addText adds a piece of text to a message item.
func (c *converter) addText(st *blockState, text string) error {
	if err := c.startItem(st); err != nil {
		return err
	}
	st.text += text
	c.text.WriteString(text)
	return c.emit("response.output_text.delta", map[string]any{
		"item_id": st.itemID, "output_index": st.outputIndex,
		"content_index": 0, "delta": text,
	})
}

// addThinking adds a piece of thinking to a reasoning item.
func (c *converter) addThinking(st *blockState, thinking string) error {
	if err := c.startItem(st); err != nil {
		return err
	}
	st.text += thinking
	c.reasoning.WriteString(thinking)
	return c.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": st.itemID, "output_index": st.outputIndex,
		"summary_index": 0, "delta": thinking,
	})
}

// blockDelta takes one piece of an upstream content block.
func (c *converter) blockDelta(ev *anthropicEvent) error {
	st, ok := c.blocks[ev.Index]
	if !ok || ev.Delta == nil {
		return nil
	}
	switch ev.Delta.Type {
	case "text_delta":
		if st.kind != "message" || ev.Delta.Text == "" {
			return nil
		}
		return c.addText(st, ev.Delta.Text)
	case "thinking_delta":
		if st.kind != "reasoning" || ev.Delta.Thinking == "" {
			return nil
		}
		return c.addThinking(st, ev.Delta.Thinking)
	case "input_json_delta":
		if st.kind != "function_call" {
			return nil
		}
		if err := c.startItem(st); err != nil {
			return err
		}
		st.args += ev.Delta.PartialJSON
		return c.emit("response.function_call_arguments.delta", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"delta": ev.Delta.PartialJSON,
		})
	}
	return nil
}

// stopBlock takes the end of one upstream content block.
func (c *converter) stopBlock(ev *anthropicEvent) error {
	st, ok := c.blocks[ev.Index]
	if !ok {
		return nil
	}
	return c.finishBlock(st, "completed")
}

// finishBlock writes the end of one output item.
//
// A block that was never started has no item and gets no end either:
// there is nothing to close.
func (c *converter) finishBlock(st *blockState, status string) error {
	if st.finished {
		return nil
	}
	st.finished = true
	if !st.started {
		return nil
	}
	switch st.kind {
	case "message":
		if err := c.emit("response.output_text.done", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"content_index": 0, "text": st.text,
		}); err != nil {
			return err
		}
		if err := c.emit("response.content_part.done", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": st.text, "annotations": []any{}},
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type": "message", "id": st.itemID, "role": "assistant",
			"status": status,
			"content": []any{map[string]any{
				"type": "output_text", "text": st.text, "annotations": []any{},
			}},
		}
		if err := c.emit("response.output_item.done", map[string]any{
			"output_index": st.outputIndex, "item": item,
		}); err != nil {
			return err
		}
		c.items[st.outputIndex] = item
	case "function_call":
		args := st.args
		if args == "" {
			// 参数是一次给全的，或者根本没有参数。
			args = toolArguments(st.input)
		}
		if err := c.emit("response.function_call_arguments.done", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"arguments": args,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type": "function_call", "id": st.itemID,
			"call_id": st.callID, "name": st.name, "arguments": args,
			"status": status,
		}
		if err := c.emit("response.output_item.done", map[string]any{
			"output_index": st.outputIndex, "item": item,
		}); err != nil {
			return err
		}
		c.items[st.outputIndex] = item
	case "reasoning":
		if err := c.emit("response.reasoning_summary_part.done", map[string]any{
			"item_id": st.itemID, "output_index": st.outputIndex,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": st.text},
		}); err != nil {
			return err
		}
		item := map[string]any{
			"type": "reasoning", "id": st.itemID,
			"summary": []any{map[string]any{"type": "summary_text", "text": st.text}},
		}
		if err := c.emit("response.output_item.done", map[string]any{
			"output_index": st.outputIndex, "item": item,
		}); err != nil {
			return err
		}
		c.items[st.outputIndex] = item
	}
	return nil
}

// closeBlocks ends the blocks that were still open.
//
// A stream that breaks in the middle leaves items that were started
// but never ended. They are closed here, so that the client does not
// wait for a done event that will never come.
func (c *converter) closeBlocks() error {
	idx := make([]int, 0, len(c.blocks))
	for i := range c.blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		if err := c.finishBlock(c.blocks[i], "incomplete"); err != nil {
			return err
		}
	}
	return nil
}

// output gives the finished items, in the order of the output.
func (c *converter) output() []any {
	idx := make([]int, 0, len(c.items))
	for i := range c.items {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]any, 0, len(idx))
	for _, i := range idx {
		out = append(out, c.items[i])
	}
	return out
}

// complete writes the end of a whole answer.
func (c *converter) complete() error {
	return c.emit("response.completed", map[string]any{
		"response": map[string]any{
			"id": c.responseID, "object": "response", "model": c.model,
			"status":                 "completed",
			"output":                 c.output(),
			"output_text":            c.text.String(),
			"reasoning_summary_text": c.reasoning.String(),
			"usage": map[string]any{
				"input_tokens": c.inputTokens, "output_tokens": c.outputTokens,
				"total_tokens": c.inputTokens + c.outputTokens,
			},
		},
	})
}

// failedError is the error of a stream that could not be finished.
type failedError struct {
	code string
	msg  string
}

func (e *failedError) Error() string { return e.msg }

// fail writes the failure of the answer and gives the error back.
//
// A failed answer gets a response.failed and no response.completed:
// the client has to know that the answer is not there.
func (c *converter) fail(code, msg string) error {
	if code == "" {
		code = "upstream_error"
	}
	if c.failed {
		// 答案已经报过一次失败了，不再报第二次。
		return &failedError{code: code, msg: msg}
	}
	c.failed = true
	if err := c.closeBlocks(); err != nil {
		return err
	}
	if err := c.emit("response.failed", map[string]any{
		"response": map[string]any{
			"id": c.responseID, "object": "response", "model": c.model,
			"status": "failed",
			"error":  map[string]any{"code": code, "message": msg},
			"output": c.output(),
			"usage": map[string]any{
				"input_tokens": c.inputTokens, "output_tokens": c.outputTokens,
				"total_tokens": c.inputTokens + c.outputTokens,
			},
		},
	}); err != nil {
		return err
	}
	return &failedError{code: code, msg: msg}
}

// emit writes one Responses event.
func (c *converter) emit(event string, payload map[string]any) error {
	payload["type"] = event
	payload["sequence_number"] = c.seq
	c.seq++
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.bw, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	return c.bw.Flush()
}

// upstreamError says what is wrong with an upstream event, if anything.
//
// MiniMax can put a business error inside a 200 answer, and it can be
// in the stream as well: as an error event, as an error field, or in
// one of MiniMax's own status fields.
func upstreamError(name string, ev *anthropicEvent) (code, msg string, bad bool) {
	if name == "error" || ev.Type == "error" {
		return "upstream_error", ev.errorMessage("the upstream said the answer failed"), true
	}
	if len(ev.Error) > 0 {
		raw := strings.TrimSpace(string(ev.Error))
		if raw != "" && raw != "null" && raw != "{}" {
			return "upstream_error", ev.errorMessage("the upstream said the answer failed"), true
		}
	}
	if si := ev.StatusInfo; si != nil && si.Code != 0 {
		m := si.Message
		if m == "" {
			m = si.Msg
		}
		if m == "" {
			m = fmt.Sprintf("status %d", si.Code)
		}
		return fmt.Sprintf("status_%d", si.Code), m, true
	}
	if br := ev.BaseResp; br != nil && br.StatusCode != 0 {
		m := br.StatusMsg
		if m == "" {
			m = fmt.Sprintf("status %d", br.StatusCode)
		}
		return fmt.Sprintf("status_%d", br.StatusCode), m, true
	}
	return "", "", false
}

// errorMessage reads the message out of an error field.
func (ev *anthropicEvent) errorMessage(fallback string) string {
	if len(ev.Error) == 0 {
		return fallback
	}
	var s string
	if json.Unmarshal(ev.Error, &s) == nil && s != "" {
		return s
	}
	var obj struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(ev.Error, &obj) == nil {
		if obj.Message != "" {
			return obj.Message
		}
		if obj.Msg != "" {
			return obj.Msg
		}
		if obj.Type != "" {
			return obj.Type
		}
	}
	return fallback
}

// toolArguments gives the arguments of a tool call as one JSON string.
//
// The Responses protocol carries the arguments as a string, so the
// pieces the upstream sends add up to one JSON string.
func toolArguments(input json.RawMessage) string {
	if len(input) == 0 {
		return "{}"
	}
	s := strings.TrimSpace(string(input))
	if s == "" || s == "null" {
		return "{}"
	}
	return s
}

// readSSE reads the upstream's SSE stream and gives each event to fn.
//
// A data field may be continued over several lines: the lines belong to
// the same payload and are joined with a newline. An event ends at the
// blank line.
func readSSE(r io.Reader, fn func(name, payload string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var name string
	var data []string
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			// 事件到这里为止
			if len(data) > 0 {
				if err := fn(name, strings.Join(data, "\n")); err != nil {
					return err
				}
			}
			name = ""
			data = nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			// 注释
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		switch field {
		case "event":
			name = strings.TrimPrefix(value, " ")
			continue
		case "data":
			data = append(data, strings.TrimPrefix(value, " "))
			continue
		case "id", "retry":
			// 跟答案没有关系
			continue
		}
		if len(data) > 0 {
			// 有些上游把一段 JSON 摊在几行上，只有第一行写了
			// data:。剩下的行是同一段话的后半截。
			data = append(data, line)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// 最后一块可能没有空行收尾
	if len(data) > 0 {
		return fn(name, strings.Join(data, "\n"))
	}
	return nil
}

// counter stands in for the random numbers if they cannot be had.
var counter atomic.Uint64

// randomID gives an id that belongs to this answer alone.
//
// The ids of the Responses protocol have to be unique: two requests
// for the same model must not answer with the same id.
func randomID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// 随机数拿不到时也不能给出重复的 id
		return fmt.Sprintf("%s_%d", prefix, counter.Add(1))
	}
	return prefix + "_" + hex.EncodeToString(b)
}
