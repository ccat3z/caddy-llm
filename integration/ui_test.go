package integration

import (
	"io"
	"net/http"
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

// TestTraceUIRoute: the traces API handler also serves the dashboard at
// {mount}/ui/ — redirect for the bare path, HTML for the app shell, and
// either the built bundle or the "not built" fallback (both are valid:
// web/dist is gitignored except its placeholder).
func TestTraceUIRoute(t *testing.T) {
	cfg := jsonConfig(
		map[string]any{
			"match": []any{map[string]any{"path": []string{"/llm/traces*"}}},
			"handle": []any{
				map[string]any{"handler": "rewrite", "strip_path_prefix": "/llm/traces"},
				map[string]any{"handler": "llm_tracer_api"},
			},
		},
	)
	cfg = strings.Replace(cfg, `"apps":{"http"`, `"apps":{"llm_tracer":{"dir":"`+t.TempDir()+`"},"http"`, 1)

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(cfg, "json")

	// /ui redirects to the trailing-slash form, keeping the mount prefix.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.Get(httpBase() + "/llm/traces/ui")
	if err != nil {
		t.Fatalf("get /ui: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Fatalf("/ui status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/llm/traces/ui/" {
		t.Errorf("/ui Location = %q, want /llm/traces/ui/", loc)
	}

	// /ui/ serves HTML — the built app when web/dist was compiled in, else
	// the not-built fallback.
	resp2, err := http.Get(httpBase() + "/llm/traces/ui/")
	if err != nil {
		t.Fatalf("get /ui/: %v", err)
	}
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if ct := resp2.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("/ui/ Content-Type = %q", ct)
	}
	built := resp2.StatusCode == 200 && strings.Contains(string(body), `id="root"`)
	fallback := resp2.StatusCode == http.StatusServiceUnavailable && strings.Contains(string(body), "not built")
	if !built && !fallback {
		t.Errorf("/ui/ = %d %s", resp2.StatusCode, body[:120])
	}

	// The API still answers on its own paths.
	resp3, err := http.Get(httpBase() + "/llm/traces?limit=1")
	if err != nil {
		t.Fatalf("get list: %v", err)
	}
	lb, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != 200 || !strings.HasPrefix(strings.TrimSpace(string(lb)), "[") {
		t.Errorf("list = %d %s", resp3.StatusCode, lb)
	}
}

// TestTracePartDownload: ?part=request|response serves one direction's raw
// bytes as an attachment.
func TestTracePartDownload(t *testing.T) {
	cfg := jsonConfig(
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
				map[string]any{"handler": "trace", "trace_name": "stub"},
				map[string]any{"handler": "static_response", "status_code": 200, "body": "pong"},
			},
		},
	)
	cfg = strings.Replace(cfg, `"apps":{"http"`, `"apps":{"llm_tracer":{"dir":"`+t.TempDir()+`"},"http"`, 1)

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(cfg, "json")

	// One traced request.
	post, err := http.Post(httpBase()+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	post.Body.Close()
	traceID := post.Header.Get("X-LLM-Trace-ID")
	if traceID == "" {
		t.Fatal("missing X-LLM-Trace-ID")
	}

	// Wait for the record, then download both parts.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(httpBase() + "/llm/traces/" + traceID + "/stub?part=request")
		if err != nil {
			t.Fatalf("get part: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, traceID+"-stub-request.log") {
				t.Errorf("Content-Disposition = %q", cd)
			}
			if !strings.Contains(string(body), `{"model":"m"`) {
				t.Errorf("request part = %q", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("request part never available: %d %s", resp.StatusCode, body)
		}
		time.Sleep(50 * time.Millisecond)
	}

	resp, err := http.Get(httpBase() + "/llm/traces/" + traceID + "/stub?part=response")
	if err != nil {
		t.Fatalf("get part: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasSuffix(string(body), "pong") {
		t.Errorf("response part = %d %q", resp.StatusCode, body)
	}

	// Bogus part rejected.
	resp2, err := http.Get(httpBase() + "/llm/traces/" + traceID + "/stub?part=bogus")
	if err != nil {
		t.Fatalf("get bogus: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("bogus part status = %d", resp2.StatusCode)
	}
}

// TestTracerDirDefault: without a configured dir, llm_tracer stores under
// caddy's data directory ($XDG_DATA_HOME/caddy/llm-tracer).
func TestTracerDirDefault(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)

	cfg := strings.Replace(jsonConfig(), `"apps":{"http"`, `"apps":{"llm_tracer":{},"http"`, 1)
	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(cfg, "json")

	want := filepath.Join(xdg, "caddy", "llm-tracer", "index.db")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("default store not created at %s: %v", want, err)
	}
}
