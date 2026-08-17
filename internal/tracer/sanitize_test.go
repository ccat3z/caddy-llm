package tracer

import (
	"net/http"
	"testing"
)

func TestRedactKey(t *testing.T) {
	cases := map[string]string{
		"sk-abcdefghijklmnop": "sk-a...mnop",
		"short":               "****",
		"1234567890123":       "1234...0123",
	}
	for in, want := range cases {
		if got := redactKey(in); got != want {
			t.Errorf("redactKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-abcdefghijklmnop")
	h.Set("X-Api-Key", "abcdefgh12345678")
	h.Set("Content-Type", "application/json")
	out := sanitizeHeaders(h)
	if out.Get("Authorization") != "Bear...mnop" {
		t.Errorf("Authorization = %q", out.Get("Authorization"))
	}
	if out.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", out.Get("Content-Type"))
	}
	if h.Get("Authorization") != "Bearer sk-abcdefghijklmnop" {
		t.Error("original header mutated")
	}
}
