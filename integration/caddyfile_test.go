package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/caddytest"

	// Register all caddy-llm modules.
	_ "github.com/ccat3z/caddy-llm/claudetoopenai"
	_ "github.com/ccat3z/caddy-llm/llmroute"
	_ "github.com/ccat3z/caddy-llm/trace"
)

// handlerNames walks a config's handler chain and returns the sequence of
// handler names, tracing into subroutes and llm_route subchains. Non-handler
// objects (servers, routes) are recursed into generically.
func handlerNames(v any) []string {
	var out []string
	switch node := v.(type) {
	case map[string]any:
		name, _ := node["handler"].(string)
		switch {
		case name == "subroute":
			out = append(out, name)
			for _, r := range routesOf(node) {
				out = append(out, handlerNames(r)...)
			}
		case name == "llm_route":
			out = append(out, name)
			for _, r := range routesOf(node["sub"]) {
				out = append(out, handlerNames(r)...)
			}
		case name != "":
			out = append(out, name)
		default:
			for _, v := range node {
				out = append(out, handlerNames(v)...)
			}
		}
	case []any:
		for _, item := range node {
			out = append(out, handlerNames(item)...)
		}
	}
	return out
}

// routesOf extracts the "routes" array from a handler object.
func routesOf(node any) []any {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	routes, _ := m["routes"].([]any)
	return routes
}

// TestCaddyfileAdapt checks the Caddyfile adapter produces the intended
// handler structure: llm_route model rules, and subchains that keep their
// written order (trace stays on its side of claude2openai) when wrapped in
// a route block.
func TestCaddyfileAdapt(t *testing.T) {
	cf := fmt.Sprintf(`{
	http_port %d
}

http://127.0.0.1:%d {
	route /v1/messages {
		llm_route {
			model glm/(.*) $1
			model glm-5.2

			route {
				rewrite * /api/coding/paas/v4/chat/completions
				claude2openai
				trace glm
				reverse_proxy open.bigmodel.cn:443
			}
		}
	}
}
`, testPorts[0], testPorts[0])

	adapted, warnings, err := (caddyfile.Adapter{ServerType: httpcaddyfile.ServerType{}}).Adapt([]byte(cf), nil)
	if err != nil {
		t.Fatalf("adapt: %v", err)
	}
	for _, w := range warnings {
		t.Errorf("adapt warning: %v", w)
	}

	var cfg map[string]any
	if err := json.Unmarshal(adapted, &cfg); err != nil {
		t.Fatalf("adapted config not JSON: %v", err)
	}

	// Find the llm_route handler and check its model rules.
	names := handlerNames(cfg)
	if !strings.Contains(strings.Join(names, ","), "llm_route") {
		t.Fatalf("no llm_route in adapted config: %v", names)
	}
	var routeBlock map[string]any
	var findLLMRoute func(v any)
	findLLMRoute = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if node["handler"] == "llm_route" {
				routeBlock = node
				return
			}
			for _, child := range node {
				findLLMRoute(child)
			}
		case []any:
			for _, item := range node {
				findLLMRoute(item)
			}
		}
	}
	findLLMRoute(cfg)
	if routeBlock == nil {
		t.Fatal("llm_route handler not found")
	}
	models, _ := routeBlock["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("llm_route models = %v", models)
	}
	if m := models[0].(map[string]any); m["pattern"] != "glm/(.*)" || m["replace"] != "$1" {
		t.Errorf("model[0] = %v", m)
	}
	if m := models[1].(map[string]any); m["pattern"] != "glm-5.2" {
		t.Errorf("model[1] = %v", m)
	}

	// The subchain keeps its written order: rewrite -> claude2openai ->
	// trace -> reverse_proxy. (The inner route block adds one subroute
	// wrapper; the rewrite's mutual-exclusion grouping is flattened by the
	// walker.)
	want := []string{"subroute", "subroute", "rewrite", "claude2openai", "trace", "reverse_proxy"}
	sub := handlerNames(routeBlock["sub"])
	if strings.Join(sub, ",") != strings.Join(want, ",") {
		t.Errorf("subchain order = %v, want %v", sub, want)
	}
}

// TestFullChainCaddyfile runs the traced translation chain configured via a
// Caddyfile — the Caddyfile twin of TestFullChainJSON.
func TestFullChainCaddyfile(t *testing.T) {
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cc-1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"caddyfile works"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`))
	}))
	defer upstream.Close()

	traceDir := t.TempDir()
	cf := fmt.Sprintf(`{
	admin localhost:%d
	http_port %d
	auto_https off

	llm_tracer %s
}

http://127.0.0.1:%d {
	route {
		trace claude
		rewrite * /v1/chat/completions
		claude2openai
		trace openai
		reverse_proxy %s
	}

	handle /llm/traces* {
		uri strip_prefix /llm/traces
		llm_tracer_api
	}
}
`, testPorts[1], testPorts[0], traceDir, testPorts[0],
		strings.TrimPrefix(upstream.URL, "http://"))

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(cf, "caddyfile")

	// Claude-format request through the whole chain.
	resp, err := http.Post(httpBase()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"client-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

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
	if txt := out["content"].([]any)[0].(map[string]any)["text"]; txt != "caddyfile works" {
		t.Errorf("content = %v", txt)
	}

	// The rewrite sent the request to the chat-completions path upstream.
	if upstreamPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", upstreamPath)
	}
}
