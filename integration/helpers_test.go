package integration

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
)

// testPorts holds the per-process random ports tests listen on, so parallel
// package runs never collide. Allocated once per test binary.
var testPorts = func() [2]int {
	for range 50 {
		l1, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			continue
		}
		p1 := l1.Addr().(*net.TCPAddr).Port
		l2, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			l1.Close()
			continue
		}
		p2 := l2.Addr().(*net.TCPAddr).Port
		l1.Close()
		l2.Close()
		return [2]int{p1, p2}
	}
	panic("no free ports for tests")
}()

// httpBase is the URL tests post to.
func httpBase() string { return fmt.Sprintf("http://127.0.0.1:%d", testPorts[0]) }

// jsonConfig builds a minimal Caddy JSON config: one HTTP server on the
// process's random port, admin likewise, no HTTPS.
func jsonConfig(routes ...map[string]any) string {
	cfg := map[string]any{
		"admin": map[string]any{"listen": fmt.Sprintf("localhost:%d", testPorts[1])},
		"apps": map[string]any{
			"http": map[string]any{
				"http_port": testPorts[0],
				"servers": map[string]any{
					"srv0": map[string]any{
						"listen":          []string{fmt.Sprintf("127.0.0.1:%d", testPorts[0])},
						"automatic_https": map[string]any{"disable": true},
						"routes":          routes,
					},
				},
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// messagesRoute wraps the handlers in a POST /v1/messages route.
func messagesRoute(handlers ...map[string]any) map[string]any {
	return map[string]any{
		"match": []any{
			map[string]any{
				"path":   []string{"/v1/messages"},
				"method": []string{"POST"},
			},
		},
		"handle": handlers,
	}
}

// llmRoute builds one llm_route handler: model rules plus a subchain of
// handlers for its upstream.
func llmRoute(rules []map[string]any, subHandlers ...map[string]any) map[string]any {
	return map[string]any{
		"handler": "llm_route",
		"models":  rules,
		"sub": map[string]any{
			"handler": "subroute",
			"routes": []any{
				map[string]any{"handle": subHandlers},
			},
		},
	}
}

// modelRule is one entry of llm_route's models list: a literal
// (replace empty) or an anchored regexp with a $1-style replacement.
func modelRule(pattern, replace string) map[string]any {
	if replace == "" {
		return map[string]any{"pattern": pattern}
	}
	return map[string]any{"pattern": pattern, "replace": replace}
}

// proxyHandler reverse-proxies to dial (host:port) with an optional
// Authorization header value.
func proxyHandler(dial, auth string) map[string]any {
	h := map[string]any{
		"handler":   "reverse_proxy",
		"upstreams": []any{map[string]any{"dial": dial}},
	}
	if auth != "" {
		h["headers"] = map[string]any{
			"request": map[string]any{
				"set": map[string]any{
					"Authorization": []string{auth},
				},
			},
		}
	}
	return h
}

// rewriteHandler sets the request path.
func rewriteHandler(uri string) map[string]any {
	return map[string]any{"handler": "rewrite", "uri": uri}
}

// claude2openaiHandler translates Anthropic↔OpenAI.
func claude2openaiHandler() map[string]any {
	return map[string]any{"handler": "claude2openai"}
}

// traceHandler captures the exchange under a trace name.
func traceHandler(name string) map[string]any {
	return map[string]any{"handler": "trace", "trace_name": name}
}

// respondHandler answers with a fixed status/body (chain terminator).
func respondHandler(status int, body string) map[string]any {
	h := map[string]any{
		"handler":     "static_response",
		"status_code": status,
	}
	if body != "" {
		h["body"] = body
	}
	return h
}

// dial extracts host:port from an httptest server URL.
func dial(srvURL string) string {
	return strings.TrimPrefix(srvURL, "http://")
}

// handleRoute wraps bare handlers in a catch-all route.
func handleRoute(handlers ...map[string]any) map[string]any {
	return map[string]any{"handle": handlers}
}

// useStoreDir points XDG_DATA_HOME at a temp dir (the llm_tracer store
// lands in caddy.AppDataDir()/llm-tracer under it) and returns that store
// dir for assertions.
func useStoreDir(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	return filepath.Join(xdg, "caddy", "llm-tracer")
}

// jsonConfigWithTracer builds the full-chain config: llm_tracer app, the
// translation chain for POST /v1/messages, and the traces API.
func jsonConfigWithTracer(upstreamURL string) string {
	cfg := map[string]any{
		"admin": map[string]any{"listen": fmt.Sprintf("localhost:%d", testPorts[1])},
		"apps": map[string]any{
			"llm_tracer": map[string]any{},
			"http": map[string]any{
				"http_port": testPorts[0],
				"servers": map[string]any{
					"srv0": map[string]any{
						"listen":          []string{fmt.Sprintf("127.0.0.1:%d", testPorts[0])},
						"automatic_https": map[string]any{"disable": true},
						"routes": []any{
							// JSON routes match strictly in order: specific
							// routes before the catch-all.
							map[string]any{
								"match": []any{
									map[string]any{
										"path":   []string{"/v1/messages"},
										"method": []string{"POST"},
									},
								},
								"handle": []any{
									traceHandler("claude"),
									rewriteHandler("/v1/chat/completions"),
									claude2openaiHandler(),
									traceHandler("openai"),
									proxyHandler(dial(upstreamURL), ""),
								},
							},
							map[string]any{
								"match": []any{map[string]any{"path": []string{"/llm/traces*"}}},
								"handle": []any{
									map[string]any{"handler": "rewrite", "strip_path_prefix": "/llm/traces"},
									map[string]any{"handler": "llm_tracer_api"},
								},
							},
							handleRoute(respondHandler(404, "")),
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	return string(b)
}
