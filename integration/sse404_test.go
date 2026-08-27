package integration

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"

	_ "github.com/ccat3z/caddy-llm/claudetoopenai"
	_ "github.com/ccat3z/caddy-llm/llmroute"
	_ "github.com/ccat3z/caddy-llm/trace"
)

// TestSSE404FallsThrough: a streaming request whose upstream answers 404
// (chunked body, like open.bigmodel.cn) must fall through, not emit an
// empty 200.
func TestSSE404FallsThrough(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"message":"no such model"}}`))
	}))
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	defer primary.Close()
	defer fallback.Close()

	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(jsonConfig(messagesRoute(
		llmRoute([]map[string]any{
			modelRule("glm-5.2", ""),
		}, claude2openaiHandler(), proxyHandler(dial(primary.URL), "")),
		llmRoute([]map[string]any{
			modelRule("glm-5.2", ""),
		}, claude2openaiHandler(), proxyHandler(dial(fallback.URL), "")),
		respondHandler(404, "no upstream"),
	)), "json")

	resp, err := http.Post(httpBase()+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	t.Logf("status=%d body=%s", resp.StatusCode, truncStr(string(b)))
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body=%s", resp.StatusCode, b)
	}
	got := string(b)
	for _, want := range []string{"event: message_start", `"text":"Hel"`, `"text":"lo"`, "event: message_stop"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream missing %q: %s", want, got)
		}
	}
}
