package integration

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"

	_ "github.com/ccat3z/caddy-llm"
)

// TestFallbackWithTranslator: primary block has rewrite_body{claude2openai}
// and fails 404; fallback must still serve. This is the pipeline-form twin of
// the plain fallthrough tests.
func TestFallbackWithTranslator(t *testing.T) {
	var primaryHits, fallbackHits atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"message":"no such model"}}`))
	}))
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"fb-ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer primary.Close()
	defer fallback.Close()

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
				model glm-5.2
				rewrite_body {
					claude2openai
				}
				route {
					reverse_proxy %s
				}
			}
			llm_route {
				model glm-5.2
				rewrite_body {
					claude2openai
				}
				route {
					reverse_proxy %s
				}
			}
			respond "no upstream" 404
		}
	}`, primary.URL, fallback.URL), "caddyfile")

	resp, err := http.Post("http://localhost:8080/v1/messages", "application/json",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("status=%d body=%s primary=%d fallback=%d", resp.StatusCode, truncStr(string(b)), primaryHits.Load(), fallbackHits.Load())
	if resp.StatusCode != 200 || !strings.Contains(string(b), "fb-ok") {
		t.Errorf("fallback not reached: %d %s", resp.StatusCode, b)
	}
	if primaryHits.Load() == 0 {
		t.Error("primary never hit")
	}
}
