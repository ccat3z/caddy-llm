package claudetoopenai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"
)

// startProxy launches a caddytest server proxying /v1/messages through
// claude2openai to the given upstream.
func startProxy(t *testing.T, upstreamURL string) *caddytest.Tester {
	t.Helper()
	tester := caddytest.NewTester(t)
	tester.InitServer(`
		{
			skip_install_trust
			admin localhost:2999
			http_port 8080
		}
		localhost:8080 {
			route {
				claude2openai
				reverse_proxy `+upstreamURL+`
			}
		}`, "caddyfile")
	return tester
}

// post posts a body to the tester's server and returns status + body.
func post(t *testing.T, tester *caddytest.Tester, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post("http://localhost:8080"+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestNonStreamingRoundTrip: Claude request -> handler -> mock OpenAI upstream
// -> translated Claude response.
func TestNonStreamingRoundTrip(t *testing.T) {
	var gotUpstreamPath string
	var gotUpstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUpstreamPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotUpstreamBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cc-1","object":"chat.completion","model":"up-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	tester := startProxy(t, upstream.URL)
	status, body := post(t, tester, "/v1/messages", `{"model":"client-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 {
		t.Fatalf("status = %d body=%s", status, body)
	}
	if gotUpstreamPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", gotUpstreamPath)
	}
	// Upstream saw the translated OpenAI request.
	if gotUpstreamBody["model"] != "client-model" {
		t.Errorf("upstream model = %v", gotUpstreamBody["model"])
	}
	msgs := gotUpstreamBody["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("upstream messages = %v", msgs)
	}
	if gotUpstreamBody["stream"] != false {
		t.Errorf("upstream stream = %v", gotUpstreamBody["stream"])
	}
	// Client got a Claude-format response.
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("client body: %v (%s)", err, body)
	}
	if out["type"] != "message" || out["model"] != "client-model" {
		t.Errorf("client body = %s", body)
	}
	content := out["content"].([]any)
	if content[0].(map[string]any)["text"] != "hello there" {
		t.Errorf("content = %v", content)
	}
}

// TestStreamingRoundTrip: Claude streaming request -> incremental SSE.
func TestStreamingRoundTrip(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, chunk := range []string{
			`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}` + "\n\n",
			`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n",
			`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, chunk)
			fl.Flush()
		}
	}))
	defer upstream.Close()

	tester := startProxy(t, upstream.URL)
	status, body := post(t, tester, "/v1/messages", `{"model":"client-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{
		"event: message_start",
		`"model":"client-model"`,
		"event: content_block_start",
		`"type":"text_delta"`,
		`"text":"Hel"`,
		`"text":"lo"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"end_turn"`,
		`"input_tokens":3`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("client stream missing %q\nstream: %s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Error("[DONE] leaked to client")
	}
}

// TestUpstreamErrorTranslation: 429 from upstream becomes a Claude error body.
func TestUpstreamErrorTranslation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit","message":"slow down"}}`))
	}))
	defer upstream.Close()

	tester := startProxy(t, upstream.URL)
	status, body := post(t, tester, "/v1/messages", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`)
	if status != 429 {
		t.Fatalf("status = %d %s", status, body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("body: %v (%s)", err, body)
	}
	e := out["error"].(map[string]any)
	if e["type"] != "rate_limit" || e["message"] != "slow down" {
		t.Errorf("error = %v", e)
	}
}

// TestCountTokensPassthrough: non-message-create paths are untouched.
func TestCountTokensPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":5}`))
	}))
	defer upstream.Close()

	tester := startProxy(t, upstream.URL)
	status, body := post(t, tester, "/v1/messages/count_tokens", `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	if status != 200 || strings.TrimSpace(body) != `{"input_tokens":5}` {
		t.Errorf("count_tokens passthrough broken: %d %s", status, body)
	}
}

// TestInvalidClaudeBody: malformed request body gets a 400 Claude error
// without reaching the next handler.
func TestInvalidClaudeBody(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
		{
			skip_install_trust
			admin localhost:2999
			http_port 8080
		}
		localhost:8080 {
			route {
				claude2openai
				respond "unreachable" 200
			}
		}`, "caddyfile")

	status, body := post(t, tester, "/v1/messages", `{not json`)
	if status != 400 {
		t.Fatalf("status = %d %s", status, body)
	}
	if !strings.Contains(body, "invalid_request_error") {
		t.Errorf("body = %s", body)
	}
}

// TestGetBodyUsable: the replaced request body must be re-readable
// (reverse_proxy retry support).
func TestGetBodyUsable(t *testing.T) {
	var reads int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	tester := startProxy(t, upstream.URL)
	status, _ := post(t, tester, "/v1/messages", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"x"}]}`)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if reads == 0 {
		t.Error("upstream never called")
	}
}
