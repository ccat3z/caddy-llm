package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddytest"

	// Register all caddy-llm modules.
	_ "github.com/ccat3z/caddy-llm/claudetoopenai"
	_ "github.com/ccat3z/caddy-llm/llmroute"
	_ "github.com/ccat3z/caddy-llm/trace"
)

// TestTraceUsageStats: traced translation chain against a mock OpenAI
// streaming upstream whose usage includes cache in prompt_tokens. The openai
// stage is configured cache_in_input, so the store must hold the three
// independent counts; the claude stage (native semantics) stores as-is.
// The usage time-series endpoint aggregates both stages.
func TestTraceUsageStats(t *testing.T) {
	// OpenAI SSE: prompt_tokens 24277 includes cached 1024.
	const openAIStream = `data: {"id":"cc","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"po"}}]}

data: {"id":"cc","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"ng"}}]}

data: {"id":"cc","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":24277,"completion_tokens":101,"prompt_tokens_details":{"cached_tokens":1024}}}

data: [DONE]
`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(openAIStream))
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
									map[string]any{"handler": "trace", "stage": "openai", "cache_in_input": true},
									map[string]any{
										"handler":    "reverse_proxy",
										"upstreams":  []any{map[string]any{"dial": strings.TrimPrefix(upstream.URL, "http://")}},
										"flush_interval": -1,
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

	resp, err := http.Post(httpBase()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body = %s", resp.StatusCode, body)
	}
	traceID := resp.Header.Get("X-LLM-Trace-ID")
	if traceID == "" {
		t.Fatal("missing X-LLM-Trace-ID")
	}

	// Poll the API until both stages are recorded with usage.
	fetchList := func() []map[string]any {
		lresp, err := http.Get(httpBase() + "/llm/traces?limit=50")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		defer lresp.Body.Close()
		lb, _ := io.ReadAll(lresp.Body)
		var entries []map[string]any
		json.Unmarshal(lb, &entries)
		return entries
	}
	var claudeStage, openaiStage map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range fetchList() {
			if e["trace_id"] != traceID {
				continue
			}
			switch e["trace_name"] {
			case "claude":
				claudeStage = e
			case "openai":
				openaiStage = e
			}
		}
		if claudeStage != nil && openaiStage != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if claudeStage == nil || openaiStage == nil {
		t.Fatalf("stages not recorded: claude=%v openai=%v", claudeStage, openaiStage)
	}

	// claude stage: translator normalized already (input excludes cache).
	// 24277 - 1024 = 23253.
	if got := claudeStage["input_tokens"]; got != float64(23253) {
		t.Errorf("claude input = %v, want 23253", got)
	}
	if got := claudeStage["cache_tokens"]; got != float64(1024) {
		t.Errorf("claude cache = %v, want 1024", got)
	}
	if got := claudeStage["output_tokens"]; got != float64(101) {
		t.Errorf("claude output = %v, want 101", got)
	}
	// openai stage: reported prompt includes cache; cache_in_input subtracts.
	if got := openaiStage["input_tokens"]; got != float64(23253) {
		t.Errorf("openai input = %v, want 23253 (normalized)", got)
	}
	if got := openaiStage["cache_tokens"]; got != float64(1024) {
		t.Errorf("openai cache = %v, want 1024", got)
	}

	// Usage time series: one bucket covering both stages.
	uresp, err := http.Get(httpBase() + "/llm/traces/usage?interval=1h")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	ub, _ := io.ReadAll(uresp.Body)
	uresp.Body.Close()
	var buckets []map[string]any
	if err := json.Unmarshal(ub, &buckets); err != nil {
		t.Fatalf("usage response not JSON: %v (%s)", err, ub)
	}
	if len(buckets) != 1 {
		t.Fatalf("buckets = %s", ub)
	}
	if buckets[0]["input_tokens"] != float64(2*23253) || buckets[0]["cache_tokens"] != float64(2*1024) {
		t.Errorf("bucket = %s", ub)
	}
	// Stage filter isolates the openai stage.
	uresp2, err := http.Get(httpBase() + "/llm/traces/usage?interval=30m&stage=openai")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	ub2, _ := io.ReadAll(uresp2.Body)
	uresp2.Body.Close()
	var buckets2 []map[string]any
	json.Unmarshal(ub2, &buckets2)
	if len(buckets2) != 1 || buckets2[0]["input_tokens"] != float64(23253) {
		t.Errorf("filtered buckets = %s", ub2)
	}
}
