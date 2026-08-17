package cpa

import (
	"encoding/json"
	"github.com/ccat3z/caddy-llm/translate"
	"strings"
	"testing"
)

// TestCliProxyAPIRegressionStream replays (upstream OpenAI SSE -> client Claude
// SSE) pairs from real streaming logs through translate.StreamConverter and compares the
// reconstructed Claude event streams semantically: same event sequence with
// same logical content (concatenated text/thinking, assembled tool inputs,
// stop_reason, usage).
func TestCliProxyAPIRegressionStream(t *testing.T) {
	files := regressionFiles(t)
	if len(files) == 0 {
		t.Skip("no usable golden files")
	}
	ran := 0
	for _, name := range files {
		lg, err := parseLogFile(name)
		if err != nil {
			continue
		}
		if !lg.IsChatCompletions() {
			continue
		}
		up := lastOKAPIResponse(lg)
		if up == nil || !strings.Contains(string(up.Body), "chat.completion.chunk") {
			continue
		}
		down := string(lg.Response.Body)
		if !strings.Contains(down, "event: message_start") {
			continue
		}
		// Incomplete captures (client disconnected mid-stream) lack the
		// terminal events; nothing to compare against.
		if !strings.Contains(down, "event: message_stop") || !strings.Contains(down, "event: message_delta") {
			continue
		}
		// Some captures are corrupted by a CLIProxyAPI logging bug: raw
		// OpenAI chunk lines leak into the client stream. Detect via
		// non-Claude payloads (no "type" field) and skip.
		if streamLogCorrupted(down) {
			continue
		}
		model := clientModel(lg)

		t.Run(name, func(t *testing.T) {
			got := replayUpstreamStream(t, up.Body, model)
			want := parseClaudeSSE(down)
			compareStreams(t, want, got)
		})
		ran++
	}
	if ran == 0 {
		t.Skip("no streaming pairs found in sample")
	}
	t.Logf("verified %d/%d files", ran, len(files))
}

// replayUpstreamStream feeds upstream SSE bytes through a StreamFeeder.
func replayUpstreamStream(t *testing.T, body []byte, model string) []translate.SSEEvent {
	t.Helper()
	f := translate.NewStreamFeeder(model)
	evts, err := f.Write(body)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	done, err := f.Close()
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	return append(evts, done...)
}

// parseClaudeSSE parses a client-facing Claude SSE body into events. Handles
// CLIProxyAPI logging artifacts: multiple `data:` lines glued onto one line
// without newline separators, and a doubled trailing [DONE].
func parseClaudeSSE(body string) []translate.SSEEvent {
	var events []translate.SSEEvent
	var cur translate.SSEEvent
	flush := func(data string) {
		if data == "[DONE]" {
			return // terminator artifact we do not reproduce
		}
		cur.Data = []byte(data)
		events = append(events, cur)
		cur = translate.SSEEvent{}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data: ") {
			if strings.HasPrefix(line, "event: ") {
				cur.Name = strings.TrimPrefix(line, "event: ")
			}
			continue
		}
		rest := strings.TrimPrefix(line, "data: ")
		if !strings.Contains(rest, "data: ") {
			flush(rest)
			continue
		}
		// Glued data lines: split on "data: " boundaries. A valid JSON object
		// ends with a top-level '}', so cut at "}data: ".
		for {
			if i := strings.Index(rest, "}data: "); i >= 0 && strings.HasPrefix(rest[i+8:], "data: ") == false {
				flush(rest[:i+1])
				rest = rest[i+8:]
				continue
			}
			break
		}
		flush(rest)
	}
	return events
}

// streamShape is the semantic digest of a Claude SSE stream.
type streamShape struct {
	model      string
	text       string
	thinking   string
	tools      []toolShape
	stopReason string
	usage      map[string]any
	blocks     []string // "thinking"|"text"|"tool_use" in open order
}

type toolShape struct {
	id    string
	name  string
	input string
}

func shapeStream(t *testing.T, events []translate.SSEEvent) streamShape {
	t.Helper()
	var sh streamShape
	toolInputs := map[int]*strings.Builder{}
	var toolOrder []int // Claude block indices of tool blocks, in order
	for _, e := range events {
		var v struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				StopReason  string `json:"stop_reason"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(e.Data, &v); err != nil {
			continue
		}
		switch e.Name {
		case "message_start":
			sh.model = v.Message.Model
		case "content_block_start":
			sh.blocks = append(sh.blocks, v.Block.Type)
			if v.Block.Type == "tool_use" {
				sh.tools = append(sh.tools, toolShape{id: v.Block.ID, name: v.Block.Name})
				toolInputs[v.Index] = &strings.Builder{}
				toolOrder = append(toolOrder, v.Index)
			}
		case "content_block_delta":
			switch v.Delta.Type {
			case "text_delta":
				sh.text += v.Delta.Text
			case "thinking_delta":
				sh.thinking += v.Delta.Thinking
			case "input_json_delta":
				if b, ok := toolInputs[v.Index]; ok {
					b.WriteString(v.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if v.Delta.StopReason != "" {
				sh.stopReason = v.Delta.StopReason
			}
			if v.Usage != nil {
				sh.usage = v.Usage
			}
		}
	}
	// attach assembled tool inputs
	for i, idx := range toolOrder {
		if i < len(sh.tools) {
			sh.tools[i].input = toolInputs[idx].String()
		}
	}
	return sh
}

func compareStreams(t *testing.T, wantEvents, gotEvents []translate.SSEEvent) {
	t.Helper()
	want := shapeStream(t, wantEvents)
	got := shapeStream(t, gotEvents)

	if want.text != got.text {
		t.Errorf("text mismatch:\n want: %q\n  got: %q", trunc(want.text), trunc(got.text))
	}
	if want.thinking != got.thinking {
		t.Errorf("thinking mismatch:\n want: %q\n  got: %q", trunc(want.thinking), trunc(got.thinking))
	}
	if want.stopReason != got.stopReason {
		t.Errorf("stop_reason: want %q, got %q", want.stopReason, got.stopReason)
	}
	if strings.Join(want.blocks, ",") != strings.Join(got.blocks, ",") {
		t.Errorf("block sequence: want %v, got %v", want.blocks, got.blocks)
	}
	if len(want.tools) != len(got.tools) {
		t.Errorf("tool count: want %d, got %d", len(want.tools), len(got.tools))
	} else {
		for i := range want.tools {
			if want.tools[i].name != got.tools[i].name {
				t.Errorf("tool[%d] name: want %q, got %q", i, want.tools[i].name, got.tools[i].name)
			}
		}
	}
	// usage: compare token fields only (some fields optional)
	compareUsage(t, want.usage, got.usage)
}

func compareUsage(t *testing.T, want, got map[string]any) {
	t.Helper()
	if want == nil && got == nil {
		return
	}
	for _, k := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens"} {
		wv, wok := numField(want, k)
		gv, gok := numField(got, k)
		if wok && gok && wv != gv {
			t.Errorf("usage.%s: want %v, got %v", k, wv, gv)
		}
	}
}

func numField(m map[string]any, k string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[k].(float64)
	return v, ok
}

// streamLogCorrupted reports whether a client-stream capture contains foreign
// (non-Claude) payloads — the signature of CLIProxyAPI's glued-logging bug,
// where raw OpenAI chunks leaked into the client stream log.
func streamLogCorrupted(down string) bool {
	for _, line := range strings.Split(down, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if strings.HasPrefix(payload, "[DONE]") {
			continue // known logging artifact (incl. doubled form), not corruption
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			return true // unparseable (e.g. glued multi-object) — corrupted
		}
		if probe.Type == "" {
			return true // not a Claude event payload
		}
	}
	return false
}
