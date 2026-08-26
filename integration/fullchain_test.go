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

	// Register all caddy-llm modules (this test uses the full chain).
	_ "github.com/ccat3z/caddy-llm"
)

// TestFullChain: listen -> llm_tracer(claude) -> claude2openai ->
// llm_tracer(openai) -> reverse_proxy, then verify both trace stages were
// recorded via the traces API.
func TestFullChain(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cc-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	traceDir := t.TempDir()
	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(jsonConfigWithTracer(traceDir, upstream.URL), "json")

	// Client request in Claude format.
	resp, err := http.Post(httpBase()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"client-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"type":"message"`) {
		t.Fatalf("client response: %d %s", resp.StatusCode, body)
	}
	traceID := resp.Header.Get("X-LLM-Trace-ID")
	if traceID == "" {
		t.Fatal("missing X-LLM-Trace-ID response header")
	}

	// Both tracer stages must be recorded (async append — poll briefly).
	// Filter to this request's trace ID: the store may hold entries from
	// earlier config reloads within the same process.
	var mine []map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for {
		lresp, err := http.Get(httpBase() + "/llm/traces")
		if err != nil {
			t.Fatalf("list traces: %v", err)
		}
		lb, _ := io.ReadAll(lresp.Body)
		lresp.Body.Close()
		var entries []map[string]any
		json.Unmarshal(lb, &entries)
		mine = nil
		for _, e := range entries {
			if e["trace_id"] == traceID {
				mine = append(mine, e)
			}
		}
		if len(mine) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(mine) != 2 {
		t.Fatalf("want 2 trace entries for %s, got %d (%s)", traceID, len(mine), mustJSON(mine))
	}
	stages := map[string]bool{}
	for _, e := range mine {
		stages[e["trace_name"].(string)] = true
	}
	if !stages["claude"] || !stages["openai"] {
		t.Errorf("stages = %v", stages)
	}

	// Full exchange bodies: claude stage saw Claude format, openai stage saw
	// OpenAI format (raw replayable HTTP messages, base64 in JSON).
	get := func(id string) map[string]any {
		gresp, err := http.Get(httpBase() + "/llm/traces/" + id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		defer gresp.Body.Close()
		gb, _ := io.ReadAll(gresp.Body)
		var e map[string]any
		json.Unmarshal(gb, &e)
		return e
	}
	b64 := func(v any) string {
		s, _ := v.(string)
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
	claudeEntry := get(traceID + "/claude")
	if cb := b64(claudeEntry["request_raw"]); !strings.HasPrefix(cb, "POST /v1/messages HTTP/1.1\r\n") || !strings.Contains(cb, `"max_tokens":10`) || !strings.Contains(cb, "\r\nHost: ") {
		t.Errorf("claude stage request_raw = %s", cb)
	}
	openaiEntry := get(traceID + "/openai")
	if ob := b64(openaiEntry["request_raw"]); !strings.Contains(ob, `"stream":false`) {
		t.Errorf("openai stage request_raw = %s", ob)
	}

	// Persistence: SQLite index + raw history file under the configured dir.
	deadline = time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(traceDir, "index.db")); err == nil {
			if matches, _ := filepath.Glob(filepath.Join(traceDir, "history-*.raw")); len(matches) > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("index.db / history-*.raw missing in %s", traceDir)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
