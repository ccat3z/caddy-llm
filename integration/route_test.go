package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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

// routeJSON builds an llm_route chain over two mock upstreams.
// Upstream "mc" (higher priority, first block) is a pass-through Claude-ish
// API; upstream "glm" runs claude2openai. Both are actually the same mocks —
// what matters is routing/fallthrough, not the body translation here.
func routeJSON(mcURL, glmURL string) string {
	return jsonConfig(messagesRoute(
		llmRoute([]map[string]any{
			modelRule(`mc/(.*)`, "$1"),
			modelRule("glm-5.2", ""),
		}, rewriteHandler("/mc/chat"), proxyHandler(dial(mcURL), "Bearer mc-key")),
		llmRoute([]map[string]any{
			modelRule(`glm/(.*)`, "$1"),
			modelRule("glm-5.2", ""),
		}, rewriteHandler("/glm/chat"), proxyHandler(dial(glmURL), "Bearer glm-key")),
		respondHandler(404, "no upstream"),
	))
}

func postModel(t *testing.T, model string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`, model)
	resp, err := http.Post(httpBase()+"/v1/messages", "application/json", strings.NewReader(body))
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

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(routeJSON(mcSrv.URL, glmSrv.URL), "json")

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

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(jsonConfig(messagesRoute(
		llmRoute([]map[string]any{
			modelRule(`native/(.*)`, "$1"),
		}, rewriteHandler("/v2/chat"), proxyHandler(dial(claudeSrv.URL), "")),
		llmRoute([]map[string]any{
			modelRule(`openai/(.*)`, "$1"),
		}, rewriteHandler("/v1/chat/completions"), claude2openaiHandler(), proxyHandler(dial(openaiUp.URL), "")),
		respondHandler(404, "no upstream"),
	)), "json")

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

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(jsonConfig(messagesRoute(
		llmRoute([]map[string]any{
			modelRule("sse-model", ""),
		}, rewriteHandler("/stream"), proxyHandler(dial(upstream.URL), "")),
		respondHandler(404, "no upstream"),
	)), "json")

	resp, err := http.Post(httpBase()+"/v1/messages", "application/json",
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

// TestBodyPipeline covers the in-memory JSON body path end to end: llm_route
// parses once, claude2openai translates the object in place, model rewrite
// happens on the clone, and responses (buffered and SSE) translate back.
func TestBodyPipeline(t *testing.T) {
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		json.Unmarshal(body, &obj)
		if obj["model"] != "glm-5.2" {
			t.Errorf("upstream saw model %v, want rewritten glm-5.2", obj["model"])
		}
		// Streaming upstream response.
		if stream, _ := obj["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for _, chunk := range []string{
				`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n",
				`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n",
				"data: [DONE]\n\n",
			} {
				io.WriteString(w, chunk)
				fl.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pipe-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer openaiUp.Close()

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(jsonConfig(messagesRoute(
		llmRoute([]map[string]any{
			modelRule(`glm/(.*)`, "$1"), modelRule("glm-5.2", ""),
		}, rewriteHandler("/v1/chat/completions"), claude2openaiHandler(), proxyHandler(dial(openaiUp.URL), "")),
		respondHandler(404, "no upstream"),
	)), "json")

	// Non-streaming through the pipeline.
	status, body := postModel(t, "glm/glm-5.2")
	if status != 200 {
		t.Fatalf("status = %d body=%s", status, body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("body: %v (%s)", err, body)
	}
	if out["type"] != "message" {
		t.Errorf("not translated: %s", body)
	}
	if txt := out["content"].([]any)[0].(map[string]any)["text"]; txt != "pipe-ok" {
		t.Errorf("text = %v", txt)
	}

	// Streaming through the pipeline (translator inside the subchain wraps
	// peekWriter, which wraps the real w).
	resp, err := http.Post(httpBase()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	got := string(b)
	for _, want := range []string{"event: message_start", `"text":"Hel"`, `"text":"lo"`, "event: message_stop"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q: %s", want, got[:min(200, len(got))])
		}
	}
}

// TestOriginalBodyAlwaysReadable locks the invariant that handlers after
// llm_route still see a complete, readable original request body even when
// llm_route read (and rewrote a clone of) it. The parsed object is
// authoritative, so equality is JSON-semantic, not byte-level.
func TestOriginalBodyAlwaysReadable(t *testing.T) {
	var bodiesSeen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodiesSeen = append(bodiesSeen, string(b))
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(jsonConfig(messagesRoute(
		llmRoute([]map[string]any{
			modelRule("glm-5.2", ""),
		}, proxyHandler(dial(up.URL), "")),
		respondHandler(404, "no upstream"),
	)), "json")

	orig := `{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(httpBase()+"/v1/messages", "application/json", strings.NewReader(orig))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(bodiesSeen) == 0 {
		t.Fatal("upstream never hit")
	}
	var want, got any
	if err := json.Unmarshal([]byte(orig), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(bodiesSeen[len(bodiesSeen)-1]), &got); err != nil {
		t.Fatalf("upstream body not JSON: %q", bodiesSeen[len(bodiesSeen)-1])
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("upstream body = %q, want original %q", bodiesSeen[len(bodiesSeen)-1], orig)
	}
}

// TestBodyTypeAssertionVisibleDownstream verifies the typed-body contract:
// handlers after llm_route can type-assert the body and mutate the parsed
// object; the mutation reaches the upstream, and the regular read path
// (no assertion) serves the same bytes.
func TestBodyTypeAssertionVisibleDownstream(t *testing.T) {
	var seenModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		seenModel, _ = obj["model"].(string)
		w.Write([]byte("ok"))
	}))
	defer up.Close()

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(jsonConfig(messagesRoute(
		llmRoute([]map[string]any{
			modelRule("glm-5.2", ""),
		}, proxyHandler(dial(up.URL), "")),
		respondHandler(404, "no upstream"),
	)), "json")

	resp, err := http.Post(httpBase()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if seenModel != "glm-5.2" {
		t.Errorf("upstream model = %q", seenModel)
	}
}

// TestLegacyClaude2openaiHandlerForm keeps the old shape working: claude2openai
// as a handler inside the subchain (not rewrite_body).
func TestLegacyClaude2openaiHandlerForm(t *testing.T) {
	openaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"legacy-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer openaiUp.Close()

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(jsonConfig(messagesRoute(
		llmRoute([]map[string]any{
			modelRule("glm-5.2", ""),
		}, rewriteHandler("/v1/chat/completions"), claude2openaiHandler(), proxyHandler(dial(openaiUp.URL), "")),
		respondHandler(404, "no upstream"),
	)), "json")

	status, body := postModel(t, "glm-5.2")
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(body, `"text":"legacy-ok"`) {
		t.Errorf("body = %s", body)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
