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

// TestInProgressVisible: an in-flight exchange appears in the list as
// in_progress and flips to completed when the chain finishes.
func TestInProgressVisible(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the response until the test lets go
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cc","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`))
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
									map[string]any{"handler": "trace", "trace_name": "slow"},
									map[string]any{
										"handler":   "reverse_proxy",
										"upstreams": []any{map[string]any{"dial": strings.TrimPrefix(upstream.URL, "http://")}},
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

	// Fire the request in the background; it blocks on the upstream.
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := http.Post(httpBase()+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	// While in flight: the row shows up as in_progress.
	findRow := func() map[string]any {
		resp, err := http.Get(httpBase() + "/llm/traces?limit=10")
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		var entries []map[string]any
		json.NewDecoder(resp.Body).Decode(&entries)
		for _, e := range entries {
			if e["trace_name"] == "slow" {
				return e
			}
		}
		return nil
	}
	deadline := time.Now().Add(3 * time.Second)
	var row map[string]any
	for time.Now().Before(deadline) {
		if row = findRow(); row != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if row == nil {
		t.Fatal("in-flight request not listed")
	}
	if row["state"] != "in_progress" {
		t.Fatalf("state = %v, want in_progress (%v)", row["state"], row)
	}
	if _, ok := row["status"]; ok {
		t.Errorf("in-progress row should have no status yet: %v", row)
	}

	// Let it finish; the same row becomes a normal completed one.
	close(release)
	<-done
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		row = findRow()
		if row != nil && row["state"] == nil && row["status"] != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if row == nil || row["state"] != nil || row["status"] != float64(200) {
		t.Fatalf("after completion: %v", row)
	}
	if row["input_tokens"] != float64(5) || row["output_tokens"] != float64(2) {
		t.Errorf("usage = %v", row)
	}
}
