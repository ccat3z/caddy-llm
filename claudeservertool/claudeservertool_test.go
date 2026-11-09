package claudeservertool

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	jsonbody "github.com/ccat3z/caddy-llm/jsonbody"
)

func TestUnsupportedTools(t *testing.T) {
	for _, kind := range []string{"web_search_20260209", "web_fetch_20260209", "code_execution_20250522", "code_execution_20260521", "tool_search_tool_regex_20251119", "tool_search_tool_bm25_20251119", "advisor_20260301"} {
		t.Run(kind, func(t *testing.T) {
			specs := serverSpecs(kind)
			calls := make([]any, 0, len(specs))
			for _, s := range specs {
				calls = append(calls, map[string]any{"type": "tool_use", "id": s.name, "name": s.name, "input": map[string]any{}})
			}
			body, _ := json.Marshal(map[string]any{"tools": []any{map[string]any{"type": kind}}, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
			rec := httptest.NewRecorder()
			err := (&ClaudeServerTool{}).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(string(body))), caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
				w.Header().Set("Content-Type", "application/json")
				return json.NewEncoder(w).Encode(map[string]any{"content": calls, "stop_reason": "tool_use"})
			}))
			if err != nil {
				t.Fatal(err)
			}
			if rec.Code != 200 {
				t.Fatalf("status %d", rec.Code)
			}
			var msg map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
				t.Fatal(err)
			}
			blocks := msg["content"].([]any)
			if len(blocks) != 2*len(specs) || msg["stop_reason"] != "end_turn" {
				t.Fatalf("response %s", rec.Body.String())
			}
			for i, s := range specs {
				call := blocks[i*2].(map[string]any)
				result := blocks[i*2+1].(map[string]any)
				if call["type"] != "server_tool_use" || result["type"] != s.result || result["tool_use_id"] != call["id"] || result["content"].(map[string]any)["error_code"] != "unavailable" {
					t.Fatalf("blocks %v", blocks)
				}
			}
		})
	}
}

func TestHistoryAndLegacyConnector(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"assistant","content":[{"type":"server_tool_use","name":"web_search","id":"srvtoolu_1","input":{"query":"hi"}}]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[]}]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"mcp_tool_use","name":"search"}]}]}`,
		`{"mcp_servers":[{"type":"url","name":"test","url":"https://example.test/mcp"}]}`,
		`{"tools":[{"type":"web_search_20260209"}]}`,
	} {
		rec := httptest.NewRecorder()
		err := (&ClaudeServerTool{}).ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), caddyhttp.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) error {
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(`{"content":[],"stop_reason":"end_turn"}`))
			return err
		}))
		if err != nil || rec.Code != 200 {
			t.Fatalf("body %s: %d %v", body, rec.Code, err)
		}
	}
}

func TestClientToolsAndPassthrough(t *testing.T) {
	for _, body := range []string{
		`{"tools":[{"name":"web_search","input_schema":{"type":"object"}}]}`,
		`{"tools":[{"type":"custom","name":"web_fetch","input_schema":{"type":"object"}}]}`,
		`{"tools":[{"type":"bash_20250124","name":"bash"},{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"},{"type":"computer_20251124","name":"computer"},{"type":"computer_toolset_20260801"},{"type":"memory_20250818","name":"memory"}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"web_search","input":{}}]},{"role":"user","content":[{"type":"tool_result","content":"ok"}]}]}`,
		`{"tools":[],"mcp_servers":[],"messages":[{"role":"user","content":"server_tool_use web_search"}]}`,
		`{"broken":`,
	} {
		for _, parsed := range []bool{false, true} {
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			if parsed {
				_, _ = jsonbody.FromBody(r)
			}
			hit := false
			err := (&ClaudeServerTool{}).ServeHTTP(httptest.NewRecorder(), r, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
				hit = true
				got, err := io.ReadAll(r.Body)
				if err != nil {
					return err
				}
				if string(got) != body {
					t.Fatalf("body changed: %s", got)
				}
				return nil
			}))
			if err != nil || !hit {
				t.Fatalf("passthrough %s: %v", body, err)
			}
		}
	}
	hit := false
	if err := (&ClaudeServerTool{}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/models", nil), caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error { hit = true; return nil })); err != nil || !hit {
		t.Fatalf("GET: %v", err)
	}
}

func TestCaddyfile(t *testing.T) {
	for _, input := range []string{"claudeservertool", "claudeservertool {\n}", "claudeservertool {\n impl unsupported\n}"} {
		h, err := parseCaddyfile(httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser(input)})
		if err != nil {
			t.Fatal(err)
		}
		handler := h.(*ClaudeServerTool)
		if err := handler.Provision(caddy.Context{}); err != nil || handler.Impl != "unsupported" {
			t.Fatalf("provision: %+v %v", handler, err)
		}
	}
	for _, input := range []string{"claudeservertool extra", "claudeservertool {\n impl\n}", "claudeservertool {\n impl unsupported extra\n}", "claudeservertool {\n unknown value\n}", "claudeservertool {\n impl unsupported\n impl unsupported\n}"} {
		if _, err := parseCaddyfile(httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser(input)}); err == nil {
			t.Fatalf("accepted invalid config: %s", input)
		}
	}
	if err := (&ClaudeServerTool{Impl: "execute"}).Provision(caddy.Context{}); err == nil {
		t.Fatal("accepted unknown impl")
	}
}
