package claudetoopenai

import (
	"bytes"
	"compress/gzip"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamingGzipHeaderDeleted locks the fix for the dead gzip branch:
// WriteHeader deletes Content-Encoding, so the streaming writer must capture
// the encoding decision BEFORE the header is dropped.
func TestStreamingGzipHeaderDeleted(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := newStreamResponseWriter(rec, "m")

	// Simulate reverse_proxy: copy upstream headers, then WriteHeader, then
	// write the (compressed) body.
	rec.Header().Set("Content-Encoding", "gzip")
	rec.Header().Set("Content-Type", "text/event-stream")
	rw.WriteHeader(200)

	// The decision must be captured even though the header is now gone.
	if !rw.gzEnabled() {
		t.Fatal("gzip flag lost: WriteHeader deleted Content-Encoding before the streaming writer captured it")
	}
	if got := rw.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding must be deleted from the client response, got %q", got)
	}
}

// TestStreamingGzipSplitMember locks the fix for per-Write gzip.Reader Reset:
// one gzip member split across multiple Write calls must still
func TestStreamingGzipSplitMember(t *testing.T) {
	var payload bytes.Buffer
	for _, chunk := range []string{
		`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}` + "\n\n",
		`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	} {
		payload.WriteString(chunk)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(payload.Bytes())
	zw.Close()
	full := gz.Bytes()

	// Split the single gzip member at an arbitrary boundary (mid-header or
	// mid-deflate — both must work).
	for _, split := range []int{4, 20, len(full) / 2, len(full) - 7} {
		t.Run("split", func(t *testing.T) {
			rec := httptest.NewRecorder()
			rw := newStreamResponseWriter(rec, "m")
			rec.Header().Set("Content-Encoding", "gzip")
			rw.WriteHeader(200)
			if !rw.gzEnabled() {
				t.Fatal("gzip not enabled")
			}
			if _, err := rw.Write(full[:split]); err != nil {
				t.Fatal(err)
			}
			if _, err := rw.Write(full[split:]); err != nil {
				t.Fatal(err)
			}
			rw.finish()

			got := rec.Body.String()
			if !strings.Contains(got, `"text":"Hel"`) || !strings.Contains(got, `"text":"lo"`) {
				t.Errorf("split at %d: translated stream incomplete: %s", split, got)
			}
			if !strings.Contains(got, "event: message_stop") {
				t.Errorf("split at %d: stream not closed: %s", split, got)
			}
		})
	}
}

// TestStreamingGzipMultiMember: several gzip members (Z_SYNC_FLUSH-style SSE)
// in separate Writes.
func TestStreamingGzipMultiMember(t *testing.T) {
	build := func(s string) []byte {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write([]byte(s))
		zw.Close()
		return b.Bytes()
	}
	rec := httptest.NewRecorder()
	rw := newStreamResponseWriter(rec, "m")
	rec.Header().Set("Content-Encoding", "gzip")
	rw.WriteHeader(200)
	for _, member := range []string{
		`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"Hel"}}]}` + "\n\n",
		`data: {"id":"s1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"lo"}}]}` + "\n\n",
		"data: [DONE]\n\n",
	} {
		if _, err := rw.Write(build(member)); err != nil {
			t.Fatal(err)
		}
	}
	rw.finish()

	got := rec.Body.String()
	if !strings.Contains(got, `"text":"Hel"`) || !strings.Contains(got, `"text":"lo"`) {
		t.Errorf("multi-member translation incomplete: %s", got)
	}
}
