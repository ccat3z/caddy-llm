package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// feedChunks feeds JSON chunk payloads and returns all emitted events.
func feedChunks(t *testing.T, c *StreamConverter, chunks ...string) []SSEEvent {
	t.Helper()
	var out []SSEEvent
	for _, ch := range chunks {
		evts, err := c.Feed([]byte(ch))
		if err != nil {
			t.Fatalf("Feed(%s): %v", trunc(ch), err)
		}
		out = append(out, evts...)
	}
	return out
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func chunk(id string, delta string, finish *string, extra ...string) string {
	var b strings.Builder
	b.WriteString(`{"id":"` + id + `","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":` + delta)
	if finish != nil {
		b.WriteString(`,"finish_reason":"` + *finish + `"`)
	}
	b.WriteString(`}]`)
	for _, e := range extra {
		b.WriteString("," + e)
	}
	b.WriteString(`}`)
	return b.String()
}

func TestStreamTextOnly(t *testing.T) {
	c := NewStreamConverter("client-model")
	evts := feedChunks(t, c,
		chunk("1", `{"role":"assistant","content":"Hello"}`, nil),
		chunk("1", `{"content":" world"}`, nil),
		chunk("1", `{}`, strPtr("stop"), `"usage":{"prompt_tokens":10,"completion_tokens":2}`),
	)
	done, _ := c.Done()
	evts = append(evts, done...)

	// message_start, text block start, 2 deltas, block stop, message_delta, message_stop
	names := eventNames(evts)
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", names)
	}
	// message_start model is the client-facing one
	var ms struct {
		Message struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal(evts[0].Data, &ms); err != nil || ms.Message.Model != "client-model" {
		t.Errorf("message_start = %s", evts[0].Data)
	}
	// text concatenated
	text := collectText(evts)
	if text != "Hello world" {
		t.Errorf("text = %q", text)
	}
	// message_delta stop_reason + usage
	var md struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(evts[len(evts)-2].Data, &md); err != nil {
		t.Fatal(err)
	}
	if md.Delta.StopReason != "end_turn" || md.Usage.InputTokens != 10 || md.Usage.OutputTokens != 2 {
		t.Errorf("message_delta = %s", evts[len(evts)-2].Data)
	}
}

func TestStreamInterleavedThinkingText(t *testing.T) {
	c := NewStreamConverter("m")
	evts := feedChunks(t, c,
		chunk("1", `{"reasoning_content":"hmm"}`, nil),
		chunk("1", `{"content":"answer"}`, nil),
		chunk("1", `{"reasoning_content":" more"}`, nil),
	)
	evts = append(evts, mustDone(t, c)...)

	// thinking block [0]: start, delta, stop; text block [1]: start, delta, stop; thinking block [2]
	var seq []string
	for _, e := range evts {
		switch e.Name {
		case "content_block_start":
			var v struct {
				Index int `json:"index"`
				Block struct {
					Type string `json:"type"`
				} `json:"content_block"`
			}
			_ = json.Unmarshal(e.Data, &v)
			seq = append(seq, "start:"+v.Block.Type+itoa(v.Index))
		case "content_block_stop":
			var v struct {
				Index int `json:"index"`
			}
			_ = json.Unmarshal(e.Data, &v)
			seq = append(seq, "stop:"+itoa(v.Index))
		}
	}
	want := "start:thinking0 stop:0 start:text1 stop:1 start:thinking2 stop:2"
	if strings.Join(seq, " ") != want {
		t.Errorf("block sequence = %v\nwant %s", seq, want)
	}
}

func TestStreamToolCallAccumulation(t *testing.T) {
	c := NewStreamConverter("m")
	// id arrives before name; arguments split across chunks
	evts := feedChunks(t, c,
		chunk("1", `{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":""}}]}`, nil),
		chunk("1", `{"tool_calls":[{"index":0,"function":{"name":"Bash","arguments":"{\"comm"}}]}`, nil),
		chunk("1", `{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"ls\"}"}}]}`, nil),
		chunk("1", `{}`, strPtr("tool_calls"), `"usage":{"prompt_tokens":5,"completion_tokens":3}`),
	)
	evts = append(evts, mustDone(t, c)...)

	text := string(EncodeAll(evts))
	// block start emitted once id+name known (chunk 2)
	if !strings.Contains(text, `"name":"Bash"`) || !strings.Contains(text, `"id":"call_1"`) {
		t.Errorf("content_block_start missing identity: %s", trunc(text))
	}
	// arguments flushed as ONE input_json_delta with complete JSON
	if got := countOccurrences(text, `"type":"input_json_delta"`); got != 1 {
		t.Errorf("input_json_delta count = %d, want 1", got)
	}
	if !strings.Contains(text, `"partial_json":"{\"command\":\"ls\"}"`) {
		t.Errorf("partial_json not assembled: %s", trunc(text))
	}
	// stop_reason forced to tool_use
	var md struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	lastDelta := findEvent(evts, "message_delta")
	if err := json.Unmarshal(lastDelta.Data, &md); err != nil || md.Delta.StopReason != "tool_use" {
		t.Errorf("stop_reason = %v (%s)", md.Delta.StopReason, lastDelta.Data)
	}
}

func TestStreamInvalidToolArgsFallback(t *testing.T) {
	c := NewStreamConverter("m")
	evts := feedChunks(t, c,
		chunk("1", `{"tool_calls":[{"index":0,"id":"x","function":{"name":"f","arguments":"not-json"}}]}`, nil),
	)
	evts = append(evts, mustDone(t, c)...)
	text := string(EncodeAll(evts))
	if !strings.Contains(text, `"partial_json":"{}"`) {
		t.Errorf("invalid args should fall back to {}: %s", trunc(text))
	}
}

func TestStreamFinishReasonCorrection(t *testing.T) {
	// finish_reason says stop but tool blocks exist -> tool_use
	c := NewStreamConverter("m")
	evts := feedChunks(t, c,
		chunk("1", `{"tool_calls":[{"index":0,"id":"x","function":{"name":"f","arguments":"{}"}}]}`, nil),
		chunk("1", `{}`, strPtr("stop")),
	)
	evts = append(evts, mustDone(t, c)...)
	var md struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	_ = json.Unmarshal(findEvent(evts, "message_delta").Data, &md)
	if md.Delta.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", md.Delta.StopReason)
	}

	// finish_reason says tool_calls but no tool blocks -> end_turn
	c2 := NewStreamConverter("m")
	evts2 := feedChunks(t, c2, chunk("1", `{"content":"hi"}`, strPtr("tool_calls")))
	evts2 = append(evts2, mustDone(t, c2)...)
	_ = json.Unmarshal(findEvent(evts2, "message_delta").Data, &md)
	if md.Delta.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", md.Delta.StopReason)
	}
}

func TestStreamEmptyStream(t *testing.T) {
	// No chunks at all: Done still produces a valid envelope.
	c := NewStreamConverter("m")
	evts := mustDone(t, c)
	names := eventNames(evts)
	want := []string{"message_start", "message_delta", "message_stop"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", names)
	}
}

func TestStreamUsageDeferredUntilDone(t *testing.T) {
	// usage arrives BEFORE finish_reason: delta must wait (no usage yet at finish)
	c := NewStreamConverter("m")
	evts := feedChunks(t, c,
		chunk("1", `{"content":"x"}`, nil, `"usage":{"prompt_tokens":7,"completion_tokens":1}`),
		chunk("1", `{}`, strPtr("stop")),
	)
	// No message_delta may be emitted before Done (usage arrived before finish).
	for _, e := range evts {
		if e.Name == "message_delta" {
			t.Fatal("message_delta emitted early")
		}
	}
	evts = append(evts, mustDone(t, c)...)
	md := findEvent(evts, "message_delta")
	if !strings.Contains(string(md.Data), `"input_tokens":7`) {
		t.Errorf("usage missing: %s", md.Data)
	}
}

func TestStreamErrorMidStream(t *testing.T) {
	c := NewStreamConverter("m")
	_ = feedChunks(t, c, chunk("1", `{"content":"par"}`, nil))
	evts := c.Error(503, []byte(`{"error":{"message":"boom"}}`))
	if len(evts) != 1 || evts[0].Name != "error" {
		t.Fatalf("events = %+v", evts)
	}
	if !strings.Contains(string(evts[0].Data), `"api_error"`) {
		t.Errorf("error data = %s", evts[0].Data)
	}
}

func TestStreamSSEScanner(t *testing.T) {
	s := &sseScanner{}
	// Partial write first.
	if err := s.Write([]byte("data: {\"a\"")); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Scan()); got != 0 {
		t.Fatalf("premature scan: %d", got)
	}
	if err := s.Write([]byte(":1}\n\nevent: x\ndata: [DONE]\n")); err != nil {
		t.Fatal(err)
	}
	lines := s.Scan()
	if len(lines) != 2 {
		t.Fatalf("lines = %+v", lines)
	}
	if string(lines[0].payload) != `{"a":1}` || lines[0].done {
		t.Errorf("line0 = %+v", lines[0])
	}
	if lines[1].payload != nil || !lines[1].done {
		t.Errorf("line1 = %+v", lines[1])
	}
}

func TestStreamNoDoneLeak(t *testing.T) {
	c := NewStreamConverter("m")
	evts := feedChunks(t, c, chunk("1", `{"content":"x"}`, strPtr("stop")))
	evts = append(evts, mustDone(t, c)...)
	if strings.Contains(string(EncodeAll(evts)), "[DONE]") {
		t.Error("[DONE] must never be forwarded to Claude clients")
	}
}

// ---------- helpers ----------

func mustDone(t *testing.T, c *StreamConverter) []SSEEvent {
	t.Helper()
	evts, err := c.Done()
	if err != nil {
		t.Fatalf("Done: %v", err)
	}
	return evts
}

func eventNames(events []SSEEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Name
	}
	return out
}

func collectText(events []SSEEvent) string {
	var sb strings.Builder
	for _, e := range events {
		if e.Name != "content_block_delta" {
			continue
		}
		var v struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(e.Data, &v) == nil && v.Delta.Type == "text_delta" {
			sb.WriteString(v.Delta.Text)
		}
	}
	return sb.String()
}

func findEvent(events []SSEEvent, name string) SSEEvent {
	for _, e := range events {
		if e.Name == name {
			return e
		}
	}
	return SSEEvent{}
}

func countOccurrences(s, sub string) int {
	return strings.Count(s, sub)
}

// trunc shortens long strings in test failure output.
func trunc(s string) string {
	const max = 600
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}
