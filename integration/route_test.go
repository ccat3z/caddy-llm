package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"

	_ "github.com/ccat3z/caddy-llm"
)

// upstreamLog records what one mock upstream received.
type upstreamLog struct {
	mu     sync.Mutex
	hits   []upstreamHit
	status int // fixed status this mock returns
}
type upstreamHit struct {
	path string
	auth string
	body map[string]any
}

func (u *upstreamLog) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		json.Unmarshal(body, &obj)
		u.mu.Lock()
		u.hits = append(u.hits, upstreamHit{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: obj})
		status := u.status
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"from-mock"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}
}

func (u *upstreamLog) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.hits)
}

func (u *upstreamLog) last() upstreamHit {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[len(u.hits)-1]
}

func (u *upstreamLog) setStatus(s int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status = s
}

// routeCaddyfile builds an llm_route chain over two mock upstreams.
// Upstream "mc" (higher priority, first block) is a pass-through Claude-ish
// API; upstream "glm" runs claude2openai. Both are actually the same mocks —
// what matters is routing/fallthrough, not the body translation here.
func routeCaddyfile(mcURL, glmURL string, mcStatus int) string {
	return fmt.Sprintf(`
	{
		skip_install_trust
		admin localhost:2999
		http_port 8080
	}
	localhost:8080 {
		@claude path /v1/messages
		route @claude {
			llm_route {
				model mc/(.*) $1
				model glm-5.2
				route {
					rewrite * /mc/chat
					reverse_proxy %s {
						header_up Authorization "Bearer mc-key"
					}
				}
			}
			llm_route {
				model glm/(.*) $1
				model glm-5.2
				route {
					rewrite * /glm/chat
					reverse_proxy %s {
						header_up Authorization "Bearer glm-key"
					}
				}
			}
			respond "no upstream" 404
		}
	}`, mcURL, glmURL)
}

func postModel(t *testing.T, model string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, model)
	resp, err := http.Post("http://localhost:8080/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestLLMRoutePriorityAndFallthrough(t *testing.T) {
	mc := &upstreamLog{status: http.StatusOK}
	glm := &upstreamLog{status: http.StatusOK}
	mcSrv := httptest.NewServer(mc.handler())
	glmSrv := httptest.NewServer(glm.handler())
	defer mcSrv.Close()
	defer glmSrv.Close()

	tester := caddytest.NewTester(t)
	tester.InitServer(routeCaddyfile(mcSrv.URL, glmSrv.URL, http.StatusOK), "caddyfile")

	t.Run("short model hits first (priority) block", func(t *testing.T) {
		status, _ := postModel(t, "glm-5.2")
		if status != 200 {
			t.Fatalf("status = %d", status)
		}
		if mc.count() != 1 || glm.count() != 0 {
			t.Fatalf("mc=%d glm=%d, want mc=1 glm=0", mc.count(), glm.count())
		}
		if h := mc.last(); h.path != "/mc/chat" || h.auth != "Bearer mc-key" {
			t.Errorf("mc hit: path=%q auth=%q", h.path, h.auth)
		}
		// model passed through unchanged (literal rule, no rewrite)
		if h := mc.last(); h.body["model"] != "glm-5.2" {
			t.Errorf("mc body model = %v", h.body["model"])
		}
	})

	t.Run("500 falls through to second upstream", func(t *testing.T) {
		mc.setStatus(http.StatusInternalServerError)
		defer mc.setStatus(http.StatusOK)
		status, body := postModel(t, "glm-5.2")
		if status != 200 {
			t.Fatalf("status = %d body=%s", status, body)
		}
		if glm.count() == 0 {
			t.Fatal("glm never hit")
		}
		if h := glm.last(); h.path != "/glm/chat" || h.auth != "Bearer glm-key" {
			t.Errorf("glm hit: path=%q auth=%q", h.path, h.auth)
		}
		// mc's Authorization must NOT leak to glm
		if h := glm.last(); h.auth == "Bearer mc-key" {
			t.Error("mc key leaked to glm")
		}
	})

	t.Run("429 falls through", func(t *testing.T) {
		mc.setStatus(http.StatusTooManyRequests)
		defer mc.setStatus(http.StatusOK)
		status, _ := postModel(t, "glm-5.2")
		if status != 200 {
			t.Fatalf("status = %d", status)
		}
	})

	t.Run("404 falls through", func(t *testing.T) {
		mc.setStatus(http.StatusNotFound)
		defer mc.setStatus(http.StatusOK)
		status, _ := postModel(t, "glm-5.2")
		if status != 200 {
			t.Fatalf("status = %d", status)
		}
	})

	t.Run("prefixed model pins first block with rewrite", func(t *testing.T) {
		status, _ := postModel(t, "mc/glm-5.2")
		if status != 200 {
			t.Fatalf("status = %d", status)
		}
		if h := mc.last(); h.body["model"] != "glm-5.2" {
			t.Errorf("prefix not stripped: model = %v", h.body["model"])
		}
	})

	t.Run("prefixed glm model goes to second block", func(t *testing.T) {
		status, _ := postModel(t, "glm/glm-5.2")
		if status != 200 {
			t.Fatalf("status = %d", status)
		}
		if h := glm.last(); h.body["model"] != "glm-5.2" {
			t.Errorf("prefix not stripped: model = %v", h.body["model"])
		}
	})

	t.Run("no match -> 404", func(t *testing.T) {
		status, body := postModel(t, "unknown-model")
		if status != 404 {
			t.Fatalf("status = %d body=%s", status, body)
		}
	})

	t.Run("both fail -> 404", func(t *testing.T) {
		mc.setStatus(http.StatusInternalServerError)
		glm.setStatus(http.StatusInternalServerError)
		defer func() { mc.setStatus(http.StatusOK); glm.setStatus(http.StatusOK) }()
		status, body := postModel(t, "glm-5.2")
		if status != 404 {
			t.Fatalf("status = %d body=%s", status, body)
		}
	})
}

// TestLLMRouteWithTranslation mixes a Claude-native upstream (no
// claude2openai) with an OpenAI upstream (claude2openai inside the block).
func TestLLMRouteWithTranslation(t *testing.T) {
	claudeUp := &upstreamLog{status: http.StatusOK}
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"translated-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		_ = body
	}))
	claudeSrv := httptest.NewServer(claudeUp.handler())
	defer claudeSrv.Close()
	defer openaiUp.Close()

	tester := caddytest.NewTester(t)
	tester.InitServer(fmt.Sprintf(`
	{
		skip_install_trust
		admin localhost:2999
		http_port 8080
	}
	localhost:8080 {
		@claude path /v1/messages
		route @claude {
			llm_route {
				model native/(.*) $1
				route {
					rewrite * /v2/chat
					reverse_proxy %s
				}
			}
			llm_route {
				model openai/(.*) $1
				route {
					rewrite * /v1/chat/completions
					claude2openai
					reverse_proxy %s
				}
			}
			respond "no upstream" 404
		}
	}`, claudeSrv.URL, openaiUp.URL), "caddyfile")

	// openai-prefixed model goes through claude2openai and comes back
	// translated as a Claude message.
	status, body := postModel(t, "openai/glm-5.2")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("body: %v (%s)", err, body)
	}
	if out["type"] != "message" {
		t.Errorf("not translated: %s", body)
	}

	// native-prefixed model bypasses translation (raw passthrough).
	status, _ = postModel(t, "native/m")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if claudeUp.count() == 0 {
		t.Error("native upstream not hit")
	}
}

// TestLLMRouteSSEStreaming verifies that a streaming (SSE) response passes
// through llm_route's peek writer progressively — chunk boundaries arrive as
// they are produced, and a mid-stream flush is forwarded.
func TestLLMRouteSSEStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, chunk := range []string{"data: {\"a\":1}\n\n", "data: {\"b\":2}\n\n", "data: [DONE]\n\n"} {
			_, _ = io.WriteString(w, chunk)
			fl.Flush()
		}
	}))
	defer upstream.Close()

	tester := caddytest.NewTester(t)
	tester.InitServer(fmt.Sprintf(`
	{
		skip_install_trust
		admin localhost:2999
		http_port 8080
	}
	localhost:8080 {
		@claude path /v1/messages
		route @claude {
			llm_route {
				model sse-model
				route {
					rewrite * /stream
					reverse_proxy %s
				}
			}
			respond "no upstream" 404
		}
	}`, upstream.URL), "caddyfile")

	resp, err := http.Post("http://localhost:8080/v1/messages", "application/json",
		strings.NewReader(`{"model":"sse-model","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	got := string(b)
	for _, want := range []string{`data: {"a":1}`, `data: {"b":2}`, "data: [DONE]"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q; got %q", want, got)
		}
	}
}
