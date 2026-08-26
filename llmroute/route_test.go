package llmroute

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestMatch(t *testing.T) {
	r := &Route{Models: []ModelRule{
		{Pattern: `mc/(.*)`, Replace: "$1"},
		{Pattern: "kimi-k3"},
		{Pattern: `glm/(.*)`, Replace: "$1"},
	}}
	// regex rules need compiled patterns (anchored, as Provision does)
	for i := range r.Models {
		m := &r.Models[i]
		if m.Replace != "" {
			re, err := regexp.Compile("^(?:" + m.Pattern + ")$")
			if err != nil {
				t.Fatal(err)
			}
			m.re = re
		}
	}
	cases := []struct {
		model     string
		matched   bool
		rewritten string
	}{
		{"mc/glm-5.2", true, "glm-5.2"},
		{"kimi-k3", true, "kimi-k3"},
		{"glm/glm-5.2", true, "glm-5.2"},
		{"glm-5.2", false, ""}, // no bare short name declared
		{"other/x", false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		rule, rewritten := r.match(c.model)
		if c.matched != (rule != nil) {
			t.Errorf("match(%q) hit=%v, want %v", c.model, rule != nil, c.matched)
			continue
		}
		if c.matched && rewritten != c.rewritten {
			t.Errorf("match(%q) rewritten=%q, want %q", c.model, rewritten, c.rewritten)
		}
	}
	// anchoring: 'mc/(.*)' must not match 'xmc/glm'
	if rule, _ := r.match("xmc/glm-5.2"); rule != nil {
		t.Error("pattern should be anchored")
	}
}

// TestPeekWriter covers the response interception logic.
func TestPeekWriter(t *testing.T) {
	t.Run("success passthrough", func(t *testing.T) {
		rec := httptest.NewRecorder()
		pw := &peekWriter{ResponseWriter: rec}
		pw.WriteHeader(200)
		pw.Write([]byte("hello"))
		if rec.Code != 200 || rec.Body.String() != "hello" {
			t.Errorf("recorder = %d %q", rec.Code, rec.Body.String())
		}
	})
	t.Run("fallthrough intercepts", func(t *testing.T) {
		rec := httptest.NewRecorder()
		pw := &peekWriter{ResponseWriter: rec}
		pw.WriteHeader(500)
		n, err := pw.Write([]byte("error body"))
		if n != 10 || err != nil {
			t.Errorf("write = %d %v", n, err)
		}
		if rec.Code != 200 || rec.Body.Len() != 0 {
			t.Errorf("real writer must stay empty, got %d %q", rec.Code, rec.Body.String())
		}
		if pw.discarded != 10 {
			t.Errorf("discarded = %d", pw.discarded)
		}
	})
	t.Run("fallthrough statuses", func(t *testing.T) {
		for code, want := range map[int]bool{
			200: false, 302: false, 400: false, 401: false, 418: false,
			404: true, 429: true, 500: true, 502: true, 503: true,
		} {
			if got := isFallthroughStatus(code); got != want {
				t.Errorf("isFallthroughStatus(%d) = %v, want %v", code, got, want)
			}
		}
	})
	t.Run("implicit 200 on write", func(t *testing.T) {
		rec := httptest.NewRecorder()
		pw := &peekWriter{ResponseWriter: rec}
		pw.Write([]byte("body-first"))
		if pw.status() != 200 {
			t.Errorf("implicit status = %d", pw.status())
		}
		if rec.Code != 200 || rec.Body.String() != "body-first" {
			t.Errorf("recorder = %d %q", rec.Code, rec.Body.String())
		}
	})
}

// TestServeHTTPFallthrough drives the handler end-to-end with a scripted
// subchain-free setup: use the exported ServeHTTP with a fake subchain via
// Provision-less Subroute? Subroute needs provisioning; instead exercise the
// routing decisions through a real caddy test later (integration). Here we
// cover the non-matching passthrough body-integrity path with a nil-safe
// guard: Route with no sub never runs the chain because match() misses.
func TestServeHTTPNoMatchPassesThrough(t *testing.T) {
	r := &Route{Models: []ModelRule{{Pattern: "kimi-k3"}}}
	var nextCalled bool
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, req *http.Request) error {
		nextCalled = true
		// body must be intact and readable
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(req.Body)
		if !strings.Contains(b.String(), `"model":"glm-5.2"`) {
			t.Errorf("body corrupted: %s", b.String())
		}
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	if err := r.ServeHTTP(rec, req, next); err != nil {
		t.Fatal(err)
	}
	if !nextCalled {
		t.Error("next not called on no-match")
	}
	if rec.Code != 200 {
		t.Errorf("status = %d", rec.Code)
	}
}
