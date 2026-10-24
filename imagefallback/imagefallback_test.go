package imagefallback

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// runFallback drives one request through a provisioned handler; next observes
// the (possibly rewritten) request.
func runFallback(t *testing.T, pattern, fallback, method, body string, next caddyhttp.HandlerFunc) {
	t.Helper()
	f := &ImageFallback{Pattern: pattern, Fallback: fallback}
	if err := f.Provision(caddy.Context{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	r := httptest.NewRequest(method, "/v1/messages", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := f.ServeHTTP(rec, r, next); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
}

// seenBody runs next and returns the request body it observed as JSON.
func seenBody(t *testing.T) (caddyhttp.HandlerFunc, *map[string]any) {
	var got map[string]any
	return func(w http.ResponseWriter, r *http.Request) error {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, b)
		}
		_, _ = w.Write([]byte("ok"))
		return nil
	}, &got
}

const imageBody = `{"model":"glm-5.3","max_tokens":10,"messages":[
	{"role":"user","content":[
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"cmVkdWN0ZWQ="}},
		{"type":"text","text":"describe this"}
	]}
]}`

// An image plus a matching model rewrites the model and keeps everything
// else — the image included — byte for byte.
func TestImageFallbackRewritesModel(t *testing.T) {
	next, got := seenBody(t)
	runFallback(t, `glm-5\.3`, "glm-5.3-flash", http.MethodPost, imageBody, next)
	m := *got
	if m["model"] != "glm-5.3-flash" {
		t.Errorf("model = %v, want glm-5.3-flash", m["model"])
	}
	if m["max_tokens"] != float64(10) {
		t.Errorf("max_tokens changed: %v", m["max_tokens"])
	}
	blocks := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["type"] != "image" {
		t.Errorf("image block altered: %v", blocks[0])
	}
	if blocks[1].(map[string]any)["text"] != "describe this" {
		t.Errorf("text block altered: %v", blocks[1])
	}
}

// An image nested in a tool_result also triggers the fallback.
func TestImageFallbackDetectsToolResultImage(t *testing.T) {
	next, got := seenBody(t)
	runFallback(t, `glm-5\.3`, "glm-5.3-flash", http.MethodPost,
		`{"model":"glm-5.3","messages":[
	{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"t1","content":[
			{"type":"image","source":{"type":"url","url":"https://example.test/x.png"}}
		]}
	]}
]}`, next)
	if m := *got; m["model"] != "glm-5.3-flash" {
		t.Errorf("model = %v, want glm-5.3-flash", m["model"])
	}
}

// Without an image, or with a non-matching model, the body is untouched.
func TestImageFallbackNoImageOrNoMatchUntouched(t *testing.T) {
	textOnly := `{"model":"glm-5.3","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	for _, tc := range []struct {
		name, pattern, body string
	}{
		{"no image, matching model", `glm-5\.3`, textOnly},
		{"image, non-matching model", `^gpt-`, imageBody},
		// Only image blocks trigger the fallback, not other media.
		{"document is not an image", `glm-5\.3`, `{"model":"glm-5.3","messages":[
	{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://example.test/y.pdf"}}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, got := seenBody(t)
			runFallback(t, tc.pattern, "glm-5.3-flash", http.MethodPost, tc.body, next)
			var want map[string]any
			if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
				t.Fatal(err)
			}
			if (*got)["model"] != want["model"] {
				t.Errorf("model = %v, want unchanged %v", (*got)["model"], want["model"])
			}
		})
	}
}

// Non-POST methods and unparseable bodies pass straight through.
func TestImageFallbackPassthrough(t *testing.T) {
	hit := false
	runFallback(t, `glm-5\.3`, "glm-5.3-flash", http.MethodGet, "", func(w http.ResponseWriter, r *http.Request) error {
		hit = true
		return nil
	})
	if !hit {
		t.Error("GET did not reach next handler")
	}

	for _, body := range []string{"{not json", "", `[1,2,3]`} {
		saw := ""
		runFallback(t, `glm-5\.3`, "glm-5.3-flash", http.MethodPost, body, func(w http.ResponseWriter, r *http.Request) error {
			b, _ := io.ReadAll(r.Body)
			saw = string(b)
			return nil
		})
		if saw != body {
			t.Errorf("unparseable body altered: got %q, want %q", saw, body)
		}
	}
}

func TestImageFallbackCaddyfile(t *testing.T) {
	h := httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser("imagefallback glm-5\\.3 glm-5.3-flash")}
	mh, err := parseCaddyfile(h)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f, ok := mh.(*ImageFallback)
	if !ok {
		t.Fatalf("wrong module type %T", mh)
	}
	if f.Pattern != `glm-5\.3` || f.Fallback != "glm-5.3-flash" {
		t.Errorf("parsed config = %+v", f)
	}
	if err := f.Provision(caddy.Context{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !f.re.MatchString("glm-5.3") || f.re.MatchString("glm-5.3-flash") {
		// The rewrite is idempotent by pattern: the fallback model should
		// not match again (here it also doesn't — the pattern is exact).
		t.Errorf("pattern matching wrong: glm-5.3=%v glm-5.3-flash=%v",
			f.re.MatchString("glm-5.3"), f.re.MatchString("glm-5.3-flash"))
	}

	for _, input := range []string{
		"imagefallback",            // no args
		"imagefallback only-model", // one arg
		"imagefallback a b extra",  // three args
	} {
		h := httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser(input)}
		if _, err := parseCaddyfile(h); err == nil {
			t.Errorf("%q should be rejected", input)
		}
	}

	// An invalid regex and a missing fallback fail provisioning.
	h = httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser("imagefallback [ glm-5.3-flash")}
	if mh, err := parseCaddyfile(h); err != nil || mh == nil {
		t.Fatalf("parse: %v (%v)", err, mh)
	} else if err := mh.(*ImageFallback).Provision(caddy.Context{}); err == nil {
		t.Error("invalid regex should fail provisioning")
	}
}
