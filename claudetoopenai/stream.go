package claudetoopenai

import (
	"encoding/json"
	"fmt"
)

// StreamConverter incrementally converts an OpenAI chat-completions SSE stream
// into Anthropic Messages API SSE events. Feed it each upstream `data:` payload
// as it arrives; on `data: [DONE]` call Done. Not safe for concurrent use.
type StreamConverter struct {
	model string

	started bool

	// content block state
	blockIndex  int    // next Claude content block index
	openKind    string // "", "thinking", or "text" — the currently open block
	sawThinking bool
	sawText     bool

	// tool call accumulation, keyed by OpenAI tool-call index
	tools   map[int]*toolAccum
	emitted bool // whether any tool content_block_start has been emitted

	// tail state
	finishReason *string
	usage        *OpenAIUsage
	sentDelta    bool // message_delta emitted
	stopped      bool // message_stop emitted
}

type toolAccum struct {
	id        string
	name      string
	nameKnown bool
	args      []byte
	blockIdx  int
	emitted   bool // content_block_start sent
}

// NewStreamConverter creates a converter for a request with the given model
// name (used in message_start).
func NewStreamConverter(model string) *StreamConverter {
	return &StreamConverter{model: model, tools: map[int]*toolAccum{}}
}

// Feed processes one upstream SSE data payload (a JSON chunk or [DONE]).
// Callers should invoke Done() when done=true is returned.
func (c *StreamConverter) Feed(payload []byte) ([]SSEEvent, error) {
	var chunk OpenAIChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		// Unparseable chunks are dropped rather than fatal — streaming must
		// survive sloppy upstreams.
		return nil, nil
	}

	var out []SSEEvent

	if chunk.Usage != nil {
		c.usage = chunk.Usage
		if c.started && c.finishReason != nil && !c.sentDelta {
			out = append(out, c.emitMessageDelta()...)
		}
	}

	for i := range chunk.Choices {
		choice := &chunk.Choices[i]

		if choice.Delta.Role != "" || choice.Delta.Content != "" || choice.Delta.ReasoningContent != "" || len(choice.Delta.ToolCalls) > 0 {
			if !c.started {
				c.started = true
				out = append(out, c.messageStart(&chunk))
			}
		}

		if d := choice.Delta.ReasoningContent; d != "" {
			evts, err := c.appendThinking(d)
			if err != nil {
				return nil, err
			}
			out = append(out, evts...)
		}
		if d := choice.Delta.Content; d != "" {
			evts, err := c.appendText(d)
			if err != nil {
				return nil, err
			}
			out = append(out, evts...)
		}
		for _, tc := range choice.Delta.ToolCalls {
			evts, err := c.appendToolCall(tc)
			if err != nil {
				return nil, err
			}
			out = append(out, evts...)
		}

		if choice.FinishReason != nil {
			c.finishReason = choice.FinishReason
		}
	}
	return out, nil
}

// Done finalizes the stream: closes open blocks, flushes accumulated tool
// arguments as a single input_json_delta each, emits the deferred message_delta
// and message_stop. Idempotent.
func (c *StreamConverter) Done() ([]SSEEvent, error) {
	if c.stopped {
		return nil, nil
	}
	c.stopped = true

	var out []SSEEvent

	// Ensure message_start exists even for empty streams.
	if !c.started {
		c.started = true
		out = append(out, c.messageStart(&OpenAIChunk{Model: c.model}))
	}

	// Close the open text/thinking block.
	if evts := c.closeOpenBlock(); len(evts) > 0 {
		out = append(out, evts...)
	}

	// Flush tool blocks in OpenAI index order.
	for _, idx := range c.sortedToolIndices() {
		t := c.tools[idx]
		if evts, err := c.flushTool(t); err == nil {
			out = append(out, evts...)
		}
	}

	if !c.sentDelta {
		out = append(out, c.emitMessageDelta()...)
	}
	out = append(out, SSEEvent{Name: "message_stop", Data: mustMarshalEvent("message_stop", map[string]any{"type": "message_stop"})})
	return out, nil
}

// Error terminates the stream with an Anthropic error event (mid-stream
// upstream failure).
func (c *StreamConverter) Error(status int, body []byte) []SSEEvent {
	return []SSEEvent{{
		Name: "error",
		Data: TranslateError(status, body),
	}}
}

// ---------- internals ----------

func (c *StreamConverter) messageStart(chunk *OpenAIChunk) SSEEvent {
	id := chunk.ID
	if id == "" {
		id = "msg_" + chunk.Model
	}
	msg := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         c.modelName(chunk),
		"content":       []any{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                0,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
			"output_tokens":               0,
		},
	}
	return SSEEvent{
		Name: "message_start",
		Data: mustMarshalEvent("message_start", map[string]any{"type": "message_start", "message": msg}),
	}
}

func (c *StreamConverter) modelName(chunk *OpenAIChunk) string {
	if c.model != "" {
		return c.model
	}
	return chunk.Model
}

func (c *StreamConverter) appendThinking(d string) ([]SSEEvent, error) {
	var out []SSEEvent
	if c.openKind != "thinking" {
		out = append(out, c.closeOpenBlock()...)
		c.openKind = "thinking"
		out = append(out, SSEEvent{
			Name: "content_block_start",
			Data: mustMarshalEvent("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         c.blockIndex,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}),
		})
		c.blockIndex++
	}
	c.sawThinking = true
	return append(out, c.blockDelta(c.blockIndex-1, map[string]any{"type": "thinking_delta", "thinking": d})), nil
}

func (c *StreamConverter) appendText(d string) ([]SSEEvent, error) {
	var out []SSEEvent
	if c.openKind != "text" {
		out = append(out, c.closeOpenBlock()...)
		c.openKind = "text"
		out = append(out, SSEEvent{
			Name: "content_block_start",
			Data: mustMarshalEvent("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         c.blockIndex,
				"content_block": map[string]any{"type": "text", "text": ""},
			}),
		})
		c.blockIndex++
	}
	c.sawText = true
	return append(out, c.blockDelta(c.blockIndex-1, map[string]any{"type": "text_delta", "text": d})), nil
}

func (c *StreamConverter) appendToolCall(tc OpenAIToolCall) ([]SSEEvent, error) {
	idx := 0
	if tc.Index != nil {
		idx = *tc.Index
	}
	t, ok := c.tools[idx]
	if !ok {
		t = &toolAccum{blockIdx: -1}
		c.tools[idx] = t
	}
	if tc.ID != "" && t.id == "" {
		t.id = tc.ID
	}
	if tc.Function.Name != "" && !t.nameKnown {
		t.name = tc.Function.Name
		t.nameKnown = true
	}
	t.args = append(t.args, tc.Function.Arguments...)

	// Emit content_block_start as soon as identity is known.
	if t.nameKnown && !t.emitted {
		t.emitted = true
		c.emitted = true
		// Opening a tool block closes an open text/thinking block first —
		// BEFORE reserving the tool's index, so the stop event carries the
		// still-open block's index (closeOpenBlock reads blockIndex-1).
		pre := c.closeOpenBlock()
		t.blockIdx = c.blockIndex
		c.blockIndex++
		return append(pre, SSEEvent{
			Name: "content_block_start",
			Data: mustMarshalEvent("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": t.blockIdx,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    t.id,
					"name":  t.name,
					"input": map[string]any{},
				},
			}),
		}), nil
	}
	return nil, nil
}

// closeOpenBlock emits content_block_stop for an open thinking/text block.
func (c *StreamConverter) closeOpenBlock() []SSEEvent {
	if c.openKind == "" {
		return nil
	}
	idx := c.blockIndex - 1
	c.openKind = ""
	return []SSEEvent{{
		Name: "content_block_stop",
		Data: mustMarshalEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx}),
	}}
}

// flushTool emits the accumulated arguments as one input_json_delta plus the
// block stop.
func (c *StreamConverter) flushTool(t *toolAccum) ([]SSEEvent, error) {
	if !t.emitted {
		return nil, nil
	}
	args := parseToolArgs(string(t.args))
	var out []SSEEvent
	out = append(out, c.blockDelta(t.blockIdx, map[string]any{
		"type":         "input_json_delta",
		"partial_json": string(args),
	}))
	out = append(out, SSEEvent{
		Name: "content_block_stop",
		Data: mustMarshalEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.blockIdx}),
	})
	return out, nil
}

func (c *StreamConverter) sortedToolIndices() []int {
	idxs := make([]int, 0, len(c.tools))
	for i := range c.tools {
		idxs = append(idxs, i)
	}
	// small n — insertion sort
	for i := 1; i < len(idxs); i++ {
		for j := i; j > 0 && idxs[j] < idxs[j-1]; j-- {
			idxs[j], idxs[j-1] = idxs[j-1], idxs[j]
		}
	}
	return idxs
}

// emitMessageDelta emits the deferred message_delta with stop_reason + usage.
func (c *StreamConverter) emitMessageDelta() []SSEEvent {
	c.sentDelta = true
	stop := "end_turn"
	if c.finishReason != nil {
		stop = mapFinishReason(*c.finishReason)
	}
	// Content wins over a lying finish_reason.
	if c.emitted {
		stop = "tool_use"
	} else if stop == "tool_use" {
		stop = "end_turn"
	}
	usage := map[string]any{
		"input_tokens":  0,
		"output_tokens": 0,
	}
	if c.usage != nil {
		cached := cachedTokens(*c.usage)
		usage = map[string]any{
			"input_tokens":            c.usage.PromptTokens - cached,
			"cache_read_input_tokens": cached,
			"output_tokens":           c.usage.CompletionTokens,
		}
	}
	return []SSEEvent{{
		Name: "message_delta",
		Data: mustMarshalEvent("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
			"usage": usage,
		}),
	}}
}

func (c *StreamConverter) blockDelta(index int, delta map[string]any) SSEEvent {
	payload := map[string]any{"type": "content_block_delta", "index": index, "delta": delta}
	return SSEEvent{Name: "content_block_delta", Data: mustMarshalEvent("content_block_delta", payload)}
}

func mustMarshalEvent(name string, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(fmt.Sprintf(`{"type":%q}`, name))
	}
	return b
}
