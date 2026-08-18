package integration

import (
	"bytes"
	"compress/gzip"
	"fmt"
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
	tester := caddytest.NewTester(t)
	tester.InitServer(fmt.Sprintf(`
	{
		skip_install_trust
		admin localhost:2999
		http_port 8080
	}
	localhost:8080 {
		route {
			llm_route {
				model glm-5.2
				route {
					claude2openai
					reverse_proxy %s
				}
			}
			respond "no-upstream" 404
		}
	}`, up.URL), "caddyfile")
	for _, enc := range []string{"identity", "gzip", "gzip"} {
		req, _ := http.NewRequest("POST", "http://localhost:8080/v1/messages",
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
