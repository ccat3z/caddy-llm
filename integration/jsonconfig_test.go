package integration

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"

	// Register all caddy-llm modules.
	_ "github.com/ccat3z/caddy-llm/claudetoopenai"
	_ "github.com/ccat3z/caddy-llm/llmroute"
	_ "github.com/ccat3z/caddy-llm/trace"
)

// TestFullChainJSON runs the same full chain as TestFullChain but configured
// via JSON instead of the Caddyfile adapter:
//
//	apps.llm_tracer (store) + route [trace(claude), claude2openai,
//	trace(openai), reverse_proxy] + route [llm_tracer_api]
func TestFullChainJSON(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &upstreamBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cc-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"json config works"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`))
	}))
	defer upstream.Close()

	traceDir := t.TempDir()
	cfg := map[string]any{
		"admin": map[string]any{"listen": fmt.Sprintf("localhost:%d", testPorts[1])},
		"apps": map[string]any{
			"llm_tracer": map[string]any{"dir": traceDir},
			"http": map[string]any{
				"http_port": testPorts[0],
				"servers": map[string]any{
					"srv0": map[string]any{
						"listen": []string{fmt.Sprintf("127.0.0.1:%d", testPorts[0])},
						"routes": []any{
							// JSON routes evaluate strictly in order (no
							// Caddyfile-style matcher reordering), so specific
							// routes must come before any catch-all.
							map[string]any{
								"match": []any{map[string]any{"path": []string{"/llm/traces*"}}},
								"handle": []any{
									map[string]any{"handler": "rewrite", "strip_path_prefix": "/llm/traces"},
									map[string]any{"handler": "llm_tracer_api"},
								},
							},
							map[string]any{
								"match": []any{map[string]any{"path": []string{"/v1/messages"}}},
								"handle": []any{
									map[string]any{"handler": "trace", "stage": "claude"},
									map[string]any{"handler": "rewrite", "uri": "/v1/chat/completions"},
									map[string]any{"handler": "claude2openai"},
									map[string]any{"handler": "trace", "stage": "openai"},
									map[string]any{
										"handler": "reverse_proxy",
										"upstreams": []any{
											map[string]any{"dial": strings.TrimPrefix(upstream.URL, "http://")},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(string(cfgJSON), "json")

	// Claude-format request through the whole chain.
	resp, err := http.Post(httpBase()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"client-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Response is translated Claude JSON.
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("client body not JSON: %v (%s)", err, body)
	}
	if out["type"] != "message" || out["model"] != "client-model" {
		t.Errorf("envelope = %s", body)
	}
	if txt := out["content"].([]any)[0].(map[string]any)["text"]; txt != "json config works" {
		t.Errorf("content = %v", txt)
	}

	// Upstream received the translated OpenAI request.
	if upstreamBody["model"] != "client-model" {
		t.Errorf("upstream model = %v", upstreamBody["model"])
	}
	if msgs, ok := upstreamBody["messages"].([]any); !ok || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("upstream messages = %v", upstreamBody["messages"])
	}

	// Both trace stages recorded under the returned trace ID.
	traceID := resp.Header.Get("X-LLM-Trace-ID")
	if traceID == "" {
		t.Fatal("missing X-LLM-Trace-ID")
	}
	waitFor := func(desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: condition not met within 3s", desc)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	fetchList := func() []map[string]any {
		lresp, err := http.Get(httpBase() + "/llm/traces")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		defer lresp.Body.Close()
		lb, _ := io.ReadAll(lresp.Body)
		var entries []map[string]any
		json.Unmarshal(lb, &entries)
		return entries
	}
	waitFor("both trace stages", func() bool {
		n := 0
		for _, e := range fetchList() {
			if e["trace_id"] == traceID {
				n++
			}
		}
		return n >= 2
	})

	// Full exchange via the API: replayable raw messages, one per stage.
	getEntry := func(stage string) map[string]any {
		gresp, err := http.Get(httpBase() + "/llm/traces/" + traceID + "/" + stage)
		if err != nil {
			t.Fatalf("get %s: %v", stage, err)
		}
		defer gresp.Body.Close()
		gb, _ := io.ReadAll(gresp.Body)
		var e map[string]any
		json.Unmarshal(gb, &e)
		return e
	}
	decodeRaw := func(v any) string {
		s, _ := v.(string)
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
	claudeMsg := decodeRaw(getEntry("claude")["request_raw"])
	if !strings.HasPrefix(claudeMsg, "POST /v1/messages HTTP/1.1\r\n") {
		t.Errorf("claude stage request_raw = %q", claudeMsg[:40])
	}
	if !strings.Contains(claudeMsg, `"max_tokens":10`) || !strings.Contains(claudeMsg, `"content":"hi"`) {
		t.Errorf("claude stage request body = %s", claudeMsg)
	}
	openaiMsg := decodeRaw(getEntry("openai")["request_raw"])
	if !strings.Contains(openaiMsg, `"stream":false`) {
		t.Errorf("openai stage request_raw = %s", openaiMsg)
	}

	// Persistence under the configured dir: SQLite index + raw history file.
	waitFor("index.db and history file", func() bool {
		if _, err := os.Stat(filepath.Join(traceDir, "index.db")); err != nil {
			return false
		}
		matches, _ := filepath.Glob(filepath.Join(traceDir, "history-*.raw"))
		return len(matches) > 0
	})
}
