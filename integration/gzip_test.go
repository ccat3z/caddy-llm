package integration

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"

	_ "github.com/ccat3z/caddy-llm"
)

// TestGzipUpstreamResponse is a regression test: upstreams that serve
// gzip-encoded responses (when the client sends Accept-Encoding: gzip) must
// still be translated — claude2openai decodes Content-Encoding before
// translating and strips the header from the client response.
func TestGzipUpstreamResponse(t *testing.T) {
	var gotAcceptEnc []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAcceptEnc = append(gotAcceptEnc, r.Header.Get("Accept-Encoding"))
		w.Header().Set("Content-Type", "application/json")
		// serve gzip-encoded body when asked, like real upstreams do
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			gz.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok-gz"},"finish_reason":"stop"}]}`))
			gz.Close()
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(buf.Bytes())
			return
		}
		w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()
	tester := caddytest.NewTester(t).WithDefaultOverrides(caddytest.Config{AdminPort: testPorts[1]})
	tester.InitServer(jsonConfig(messagesRoute(
		llmRoute([]map[string]any{
			modelRule("glm-5.2", ""),
		}, claude2openaiHandler(), proxyHandler(dial(up.URL), "")),
		respondHandler(404, "no-upstream"),
	)), "json")
	for _, enc := range []string{"identity", "gzip", "gzip"} {
		req, _ := http.NewRequest("POST", httpBase()+"/v1/messages",
			strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", enc)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Logf("enc=%s: client error %v", enc, err)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("enc=%s -> %d %s", enc, resp.StatusCode, truncStr(string(b)))
	}
	if len(gotAcceptEnc) == 0 {
		t.Fatal("upstream never hit")
	}
}

func truncStr(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
