package translate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/ccat3z/caddy-llm/internal/llmtestutil/logparse"
)

// TestGoldenResponseTranslation replays (upstream OpenAI response -> client
// Claude response) pairs from non-streaming real logs through
// TranslateResponse and compares semantically.
func TestGoldenResponseTranslation(t *testing.T) {
	files := goldenFiles(t)
	if len(files) == 0 {
		t.Skip("no usable golden files")
	}
	ran := 0
	for _, name := range files {
		lg, err := parseLogFile(name)
		if err != nil {
			continue
		}
		if !lg.IsChatCompletions() || len(lg.APIResponses) == 0 || len(lg.Response.Body) == 0 {
			continue
		}
		// Non-streaming only here: upstream JSON chat.completion, client JSON message.
		up := lastOKAPIResponse(lg)
		if up == nil || !json.Valid(up.Body) {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(up.Body, &obj); err != nil || obj["object"] != "chat.completion" {
			continue
		}
		if strings.Contains(string(lg.Response.Body), "message_start") {
			continue // streaming pair, covered by stream golden tests
		}
		model := clientModel(lg)

		t.Run(name, func(t *testing.T) {
			var in OpenAIResponse
			if err := json.Unmarshal(up.Body, &in); err != nil {
				t.Skipf("unparseable upstream body: %v", err)
			}
			got, err := json.Marshal(TranslateResponse(&in, model))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Content-block ORDER differs from CLIProxyAPI (it puts text
			// before thinking; we emit thinking first, which matches
			// Anthropic semantics). Compare as block multisets.
			assertJSONDiffEq(t, "translated response",
				sortContentBlocks(lg.Response.Body), sortContentBlocks(got))
		})
		ran++
	}
	if ran == 0 {
		t.Skip("no non-streaming pairs found in sample")
	}
	t.Logf("verified %d/%d files", ran, len(files))
}

// TestGoldenErrorTranslation replays upstream error responses through
// TranslateError and compares against the client-facing error body. Errors are
// rare in the corpus, so this scans the full log directory for error-final
// files rather than the standard sample.
func TestGoldenErrorTranslation(t *testing.T) {
	files := goldenFiles(t)
	if len(files) == 0 {
		t.Skip("no usable golden files")
	}
	// Known error-final files (verified against the corpus — files whose final
	// upstream attempt returned a real 4xx/5xx body) so the test finds cases
	// even when the deterministic sample contains none. Excluded: files whose
	// last attempt died with a transport "Error:" line (no upstream response).
	files = append(files, []string{
		"v1-messages-2026-07-06T220142-a3fd0aa6.log",
		"v1-messages-2026-07-10T085330-596af4d1.log",
		"v1-messages-2026-07-10T091101-d4b61520.log",
		"v1-messages-2026-07-10T093138-30459bea.log",
		"v1-messages-2026-08-06T103044-22b67f1e.log",
	}...)
	ran := 0
	for _, name := range files {
		lg, err := parseLogFile(name)
		if err != nil {
			continue
		}
		if !lg.IsChatCompletions() || len(lg.APIResponses) == 0 {
			continue
		}
		// Only compare when the FINAL attempt errored: with retry chains the
		// client sees the last attempt's outcome, not earlier failures. Files
		// whose last attempt has no status (transport "Error:" capture, no
		// upstream response at all) are out of scope for translation tests.
		if lg.APIResponses[len(lg.APIResponses)-1].Status == 0 {
			continue
		}
		up := lastAPIResponse(lg)
		if up == nil || up.Status < 400 || lg.Response.Status < 400 {
			continue
		}
		t.Run(name, func(t *testing.T) {
			got := TranslateError(up.Status, up.Body)
			assertJSONDiffEq(t, "translated error", lg.Response.Body, got)
		})
		ran++
	}
	if ran == 0 {
		t.Skip("no error responses found in sample")
	}
	t.Logf("verified %d/%d files", ran, len(files))
}

// sortContentBlocks canonicalizes a response body's content block order
// (sorts blocks by type then text) so order-only differences don't fail
// comparison.
func sortContentBlocks(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	blocksRaw, ok := m["content"]
	if !ok {
		return body
	}
	var blocks []map[string]any
	if err := json.Unmarshal(blocksRaw, &blocks); err != nil || len(blocks) < 2 {
		return body
	}
	sort.Slice(blocks, func(i, j int) bool {
		ti, tj := blockSortKey(blocks[i]), blockSortKey(blocks[j])
		if ti != tj {
			return ti < tj
		}
		return fmt.Sprint(blocks[i]["text"], blocks[i]["thinking"]) <
			fmt.Sprint(blocks[j]["text"], blocks[j]["thinking"])
	})
	m["content"] = mustMarshal(blocks)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func blockSortKey(b map[string]any) string {
	t, _ := b["type"].(string)
	return t
}

// lastOKAPIResponse returns the last 2xx upstream response, or nil.
func lastOKAPIResponse(lg *logparse.Log) *logparse.APIResponse {
	for i := len(lg.APIResponses) - 1; i >= 0; i-- {
		if lg.APIResponses[i].Status >= 200 && lg.APIResponses[i].Status < 300 {
			return &lg.APIResponses[i]
		}
	}
	return nil
}

// lastAPIResponse returns the last upstream response with a real HTTP status,
// or nil. (Some files contain an empty un-numbered "=== API RESPONSE ==="
// section that parses to status 0; skip those.)
func lastAPIResponse(lg *logparse.Log) *logparse.APIResponse {
	for i := len(lg.APIResponses) - 1; i >= 0; i-- {
		if lg.APIResponses[i].Status > 0 {
			return &lg.APIResponses[i]
		}
	}
	return nil
}

// clientModel extracts the model name from the client request body.
func clientModel(lg *logparse.Log) string {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(lg.RequestBody, &req)
	return req.Model
}
