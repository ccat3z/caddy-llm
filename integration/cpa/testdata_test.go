package cpa

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ccat3z/caddy-llm/internal"
	"github.com/ccat3z/caddy-llm/internal/trans"
	"github.com/ccat3z/caddy-llm/translate"
)

// The testdata cases are sanitized extracts from the real CLIProxyAPI corpus
// (credentials, cookies, session IDs, hostnames, and user paths redacted).
// Unlike the corpus tests above, these run everywhere — no external directory
// required.

func loadCase(t *testing.T, name string) *Log {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()
	lg, err := Parse(f)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if !lg.IsChatCompletions() {
		t.Fatalf("%s: expected chat-completions upstream", name)
	}
	return lg
}

// TestTestdataRequestTranslation checks request translation on every
// request/response-intact case (error.log's request body was shrunk during
// sanitization, so it is response-side only).
func TestTestdataRequestTranslation(t *testing.T) {
	for _, name := range []string{"stream_text.log", "stream_tool_use.log", "stream_thinking.log", "nonstream_tool_use.log"} {
		t.Run(name, func(t *testing.T) {
			lg := loadCase(t, name)
			in := &internal.LazyJsonNode{Val: json.RawMessage(lg.RequestBody)}
			got, err := trans.TranslateRequest(in)
			if err != nil {
				t.Fatalf("TranslateRequest: %v", err)
			}
			out, err := got.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			golden := normalizeGoldenOverrides(lg.APIRequests[0].Body, lg.RequestBody)
			assertJSONDiffEq(t, "translated request", golden, out)
		})
	}
}

// TestTestdataStreamText: simple text-only stream.
func TestTestdataStreamText(t *testing.T) {
	lg := loadCase(t, "stream_text.log")
	got := replayUpstreamStream(t, lastOKAPIResponse(lg).Body, clientModel(lg))
	want := parseClaudeSSE(string(lg.Response.Body))
	if len(want) == 0 {
		t.Fatal("no events parsed from golden")
	}
	compareStreams(t, want, got)
}

// TestTestdataStreamToolUse: streaming tool-call assembly.
func TestTestdataStreamToolUse(t *testing.T) {
	lg := loadCase(t, "stream_tool_use.log")
	got := replayUpstreamStream(t, lastOKAPIResponse(lg).Body, clientModel(lg))
	want := parseClaudeSSE(string(lg.Response.Body))
	compareStreams(t, want, got)

	// The tool case must actually exercise tool blocks.
	gotShape := shapeStream(t, got)
	if len(gotShape.tools) == 0 {
		t.Error("expected tool_use blocks in this case")
	}
}

// TestTestdataStreamThinking: reasoning_content -> thinking blocks.
func TestTestdataStreamThinking(t *testing.T) {
	lg := loadCase(t, "stream_thinking.log")
	got := replayUpstreamStream(t, lastOKAPIResponse(lg).Body, clientModel(lg))
	want := parseClaudeSSE(string(lg.Response.Body))
	compareStreams(t, want, got)

	gotShape := shapeStream(t, got)
	if gotShape.thinking == "" {
		t.Error("expected thinking content in this case")
	}
}

// TestTestdataNonStreamToolUse: buffered response translation with tool_calls.
func TestTestdataNonStreamToolUse(t *testing.T) {
	lg := loadCase(t, "nonstream_tool_use.log")
	up := lastOKAPIResponse(lg)
	var in translate.OpenAIResponse
	if err := json.Unmarshal(up.Body, &in); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	got, err := json.Marshal(translate.TranslateResponse(&in, clientModel(lg)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"tool_use"`) {
		t.Errorf("expected tool_use block: %s", trunc(string(got)))
	}
	assertJSONDiffEq(t, "translated response",
		sortContentBlocks(lg.Response.Body), sortContentBlocks(got))
}

// TestTestdataError: mid-stream upstream error (HTTP 200 + SSE stream that
// terminates with an error payload) becomes a Claude `event: error`.
func TestTestdataError(t *testing.T) {
	lg := loadCase(t, "error.log")

	// The upstream responded 200 with an SSE stream, but the stream carries an
	// OpenAI error object; CLIProxyAPI translated it to a client-facing error.
	down := string(lg.Response.Body)
	if !strings.Contains(down, "event: error") {
		t.Fatalf("expected mid-stream error in client response: %s", trunc(down[:400]))
	}
	if lg.Response.Status != 502 {
		t.Errorf("client status = %d, want 502", lg.Response.Status)
	}

	// Extract the upstream error payload from the stream tail: the last
	// complete JSON error object in the upstream body.
	up := lastOKAPIResponse(lg)
	var upstreamErr string
	for _, m := range strings.Split(string(up.Body), "\n") {
		if strings.HasPrefix(m, `{"error":`) {
			upstreamErr = m
		}
	}
	if upstreamErr == "" {
		t.Fatal("no upstream error payload found")
	}

	// TranslateError must reproduce the client-facing error envelope.
	got := translate.TranslateError(502, []byte(upstreamErr))
	var out map[string]any
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("error body: %v (%s)", err, got)
	}
	if out["type"] != "error" {
		t.Errorf("type = %v", out["type"])
	}
	e := out["error"].(map[string]any)
	if e["message"] == "" {
		t.Error("empty error message")
	}
}

// TestTestdataNoSensitiveData guards the sanitization of the committed
// fixtures: none of the redacted fields may leak.
func TestTestdataNoSensitiveData(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{
		"sankuai", "bigmodel", "ccat3z",
		"acw_tc=76b", // raw cookie prefix seen in the corpus
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			s := string(b)
			for _, p := range banned {
				if strings.Contains(s, p) {
					t.Errorf("sanitization leak: %q present", p)
				}
			}
		})
	}
}
