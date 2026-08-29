package cpa_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// logsDir and sample reuse the package-level corpus directory (see
// request_test.go; controlled by CPA_LOG_DIR).

func sample(name string) string { return filepath.Join(logsDir, name) }

func TestParseCountTokens(t *testing.T) {
	if _, err := os.Stat(logsDir); err != nil {
		t.Skipf("logs dir not available: %v", err)
	}
	f, err := os.Open(sample("v1-messages-count_tokens-2026-07-05T183259-5b0bd380.log"))
	if err != nil {
		t.Skipf("sample file not available: %v", err)
	}
	defer f.Close()

	lg, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := lg.Info["URL"]; got != "/v1/messages/count_tokens?beta=true" {
		t.Errorf("Info[URL] = %q", got)
	}
	if lg.Headers.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("missing client header Anthropic-Version: %v", lg.Headers)
	}
	if !json.Valid(lg.RequestBody) {
		t.Errorf("request body not valid JSON")
	}
	// count_tokens requests are answered by the proxy itself: no upstream attempt.
	if len(lg.APIRequests) != 0 || len(lg.APIResponses) != 0 {
		t.Errorf("count_tokens should have no upstream attempts, got %d requests / %d responses",
			len(lg.APIRequests), len(lg.APIResponses))
	}
	if lg.IsChatCompletions() {
		t.Error("no upstream attempts => not chat completions")
	}
	if lg.Response.Status != 200 {
		t.Errorf("response status = %d", lg.Response.Status)
	}
	var body map[string]any
	if err := json.Unmarshal(lg.Response.Body, &body); err != nil {
		t.Fatalf("response body: %v", err)
	}
	if body["input_tokens"] != float64(15596) {
		t.Errorf("input_tokens = %v", body["input_tokens"])
	}
}

func TestParseStreamingRoundTrip(t *testing.T) {
	if _, err := os.Stat(logsDir); err != nil {
		t.Skipf("logs dir not available: %v", err)
	}
	f, err := os.Open(sample("v1-messages-2026-07-11T090620-26946624.log"))
	if err != nil {
		t.Skipf("sample file not available: %v", err)
	}
	defer f.Close()

	lg, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(lg.APIRequests) != 1 || len(lg.APIResponses) != 1 {
		t.Fatalf("want 1 upstream attempt, got %d requests / %d responses",
			len(lg.APIRequests), len(lg.APIResponses))
	}
	if !lg.IsChatCompletions() {
		t.Errorf("upstream URL %q should be chat completions", lg.APIRequests[0].UpstreamURL)
	}
	// Upstream request body is single-line JSON in OpenAI format.
	var req map[string]any
	if err := json.Unmarshal(lg.APIRequests[0].Body, &req); err != nil {
		t.Fatalf("api request body: %v", err)
	}
	if req["model"] != "glm-5.2" {
		t.Errorf("upstream model = %v", req["model"])
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 || msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("first upstream message should be system, got %v", msgs[0])
	}
	// Upstream response is raw SSE.
	up := string(lg.APIResponses[0].Body)
	if !strings.Contains(up, "data: {") || !strings.Contains(up, "chat.completion.chunk") {
		t.Error("upstream response body should be OpenAI SSE")
	}
	// Client-facing response is Claude SSE.
	down := string(lg.Response.Body)
	for _, ev := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(down, "event: "+ev) {
			t.Errorf("client response missing %s event", ev)
		}
	}
	// Known CLIProxyAPI logging artifact: doubled terminator. The parser must
	// surface it verbatim so tests can assert we do NOT reproduce it.
	if !strings.HasSuffix(strings.TrimRight(down, "\n"), "data: [DONE]data: [DONE]") {
		t.Error("expected doubled [DONE] artifact at end of client stream")
	}
}

func TestParseNonStreamingToolUse(t *testing.T) {
	if _, err := os.Stat(logsDir); err != nil {
		t.Skipf("logs dir not available: %v", err)
	}
	f, err := os.Open(sample("v1-messages-2026-07-11T104925-7aa72131.log"))
	if err != nil {
		t.Skipf("sample file not available: %v", err)
	}
	defer f.Close()

	lg, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if lg.APIResponses[0].Status != 200 {
		t.Errorf("upstream status = %d", lg.APIResponses[0].Status)
	}
	var up map[string]any
	if err := json.Unmarshal(lg.APIResponses[0].Body, &up); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	if up["object"] != "chat.completion" {
		t.Errorf("upstream object = %v", up["object"])
	}
	var down map[string]any
	if err := json.Unmarshal(lg.Response.Body, &down); err != nil {
		t.Fatalf("client body: %v", err)
	}
	if down["type"] != "message" {
		t.Errorf("client type = %v", down["type"])
	}
	// Should contain a tool_use content block.
	found := false
	for _, b := range down["content"].([]any) {
		if b.(map[string]any)["type"] == "tool_use" {
			found = true
		}
	}
	if !found {
		t.Error("client response should contain a tool_use block")
	}
}

func TestParseSynthetic(t *testing.T) {
	// Covers multi-attempt retry and unknown-section tolerance without
	// depending on external files.
	in := `=== REQUEST INFO ===
Version: dev
URL: /v1/messages
Method: POST

=== HEADERS ===
Authorization: Bearer sk-x
Vary: a
Vary: b

=== REQUEST BODY ===
{"model":"m","stream":true}

=== API REQUEST 1 ===
Timestamp: t1
Upstream URL: https://u.example/v1/chat/completions

Headers:
Accept: text/event-stream

Body:
{"model":"m"}

=== API REQUEST 2 ===
Upstream URL: https://u.example/v1/chat/completions

Body:
{"model":"m"}

=== API RESPONSE 2 ===
Timestamp: t2

Status: 200
Headers:
Content-Type: text/event-stream

Body:
data: {"x":1}

data: [DONE]

=== FUTURE SECTION ===
whatever

=== RESPONSE ===
Status: 200
Content-Type: text/event-stream

event: message_start
data: {"type":"message_start"}
`
	lg, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(lg.APIRequests) != 2 {
		t.Fatalf("want 2 API requests, got %d", len(lg.APIRequests))
	}
	if lg.APIRequests[0].N != 1 || lg.APIRequests[1].N != 2 {
		t.Errorf("attempt numbers = %d, %d", lg.APIRequests[0].N, lg.APIRequests[1].N)
	}
	if len(lg.APIResponses) != 1 || lg.APIResponses[0].N != 2 {
		t.Fatalf("want 1 API response (N=2), got %+v", lg.APIResponses)
	}
	if got := lg.APIRequests[0].Headers.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q", got)
	}
	if !lg.IsChatCompletions() {
		t.Error("should be chat completions")
	}
	body := string(lg.APIResponses[0].Body)
	if !strings.HasPrefix(body, "data: {\"x\":1}") || !strings.HasSuffix(body, "data: [DONE]") {
		t.Errorf("SSE body round-trip mismatch: %q", body)
	}
	if lg.Response.Headers.Get("Content-Type") != "text/event-stream" {
		t.Errorf("response Content-Type = %q", lg.Response.Headers.Get("Content-Type"))
	}
	if !strings.Contains(string(lg.Response.Body), "event: message_start") {
		t.Errorf("response body = %q", lg.Response.Body)
	}
}
