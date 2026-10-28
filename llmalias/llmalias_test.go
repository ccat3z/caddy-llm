package llmalias

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// runAlias drives one request through a provisioned handler; next observes
// the (possibly rewritten) request and returns the raw body it saw.
func runAlias(t *testing.T, find, replace, method, body string) string {
	t.Helper()
	a := &LLMAlias{Find: find, Replace: replace}
	if err := a.Provision(caddy.Context{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	saw := ""
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		saw = string(b)
		_, _ = w.Write([]byte("ok"))
		return nil
	})
	r := httptest.NewRequest(method, "/v1/messages", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := a.ServeHTTP(rec, r, next); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	return saw
}

const modelBody = `{"model":"glm-5.3","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`

func TestLLMAliasWholeModel(t *testing.T) {
	// Anchored pattern replaces the whole model name.
	got := runAlias(t, `^glm-5\.3$`, "glm-5.3-air", http.MethodPost, modelBody)
	if !strings.Contains(got, `"model":"glm-5.3-air"`) {
		t.Errorf("model not rewritten: %s", got)
	}
	if !strings.Contains(got, `"max_tokens":10`) {
		t.Errorf("other fields changed: %s", got)
	}
}

func TestLLMAliasCaptureGroups(t *testing.T) {
	got := runAlias(t, `^glm/(.+)$`, "glm-$1", http.MethodPost,
		`{"model":"glm/4.7-flash","messages":[]}`)
	if !strings.Contains(got, `"model":"glm-4.7-flash"`) {
		t.Errorf("capture group not applied: %s", got)
	}
}

// Unanchored patterns replace just the matched substring, like Caddy's
// rewrite regex replacer.
func TestLLMAliasUnanchoredSubstring(t *testing.T) {
	got := runAlias(t, `-mini$`, "-max", http.MethodPost,
		`{"model":"glm-5.3-mini","messages":[]}`)
	if !strings.Contains(got, `"model":"glm-5.3-max"`) {
		t.Errorf("substring not replaced: %s", got)
	}
}

// A non-matching model, a missing model member, non-POST methods, and
// unparseable bodies all pass through untouched.
func TestLLMAliasPassthrough(t *testing.T) {
	cases := []struct {
		name, find, replace, method, body string
	}{
		{"non-matching model", `^gpt-`, "gpt-4o", http.MethodPost, modelBody},
		{"no model member", `^glm`, "x", http.MethodPost, `{"messages":[]}`},
		{"GET", `^glm`, "x", http.MethodGet, modelBody},
		{"bad body", `^glm`, "x", http.MethodPost, "{not json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runAlias(t, tc.find, tc.replace, tc.method, tc.body)
			if tc.body == "{not json" || tc.method != http.MethodPost {
				// These never go through the JSON body, so the bytes are
				// literally unchanged.
				if got != tc.body {
					t.Errorf("body altered: got %q, want %q", got, tc.body)
				}
				return
			}
			// Parsed bodies are re-marshaled on their way through, which
			// reorders keys; compare semantically.
			var gotV, wantV any
			if err := json.Unmarshal([]byte(got), &gotV); err != nil {
				t.Fatalf("got not JSON: %v", err)
			}
			if err := json.Unmarshal([]byte(tc.body), &wantV); err != nil {
				t.Fatalf("want not JSON: %v", err)
			}
			if !reflect.DeepEqual(gotV, wantV) {
				t.Errorf("body altered: got %s, want %s", got, tc.body)
			}
		})
	}
}

func TestLLMAliasCaddyfile(t *testing.T) {
	h := httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser(`llm_alias ^glm/(.+)$ glm-$1`)}
	mh, err := parseCaddyfile(h)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a, ok := mh.(*LLMAlias)
	if !ok {
		t.Fatalf("wrong module type %T", mh)
	}
	if a.Find != `^glm/(.+)$` || a.Replace != "glm-$1" {
		t.Errorf("parsed config = %+v", a)
	}
	if err := a.Provision(caddy.Context{}); err != nil {
		t.Fatalf("provision: %v", err)
	}

	for _, input := range []string{
		"llm_alias",           // no args
		"llm_alias only-find", // one arg
		"llm_alias a b extra", // three args
	} {
		h := httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser(input)}
		if _, err := parseCaddyfile(h); err == nil {
			t.Errorf("%q should be rejected", input)
		}
	}

	// An invalid regex and an empty find fail provisioning.
	h = httpcaddyfile.Helper{Dispenser: caddyfile.NewTestDispenser("llm_alias [ glm")}
	if mh, err := parseCaddyfile(h); err != nil || mh == nil {
		t.Fatalf("parse: %v (%v)", err, mh)
	} else if err := mh.(*LLMAlias).Provision(caddy.Context{}); err == nil {
		t.Error("invalid regex should fail provisioning")
	}
	if err := (&LLMAlias{Replace: "x"}).Provision(caddy.Context{}); err == nil {
		t.Error("empty find should fail provisioning")
	}
}
