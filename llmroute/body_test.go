package llmroute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadSemantics(t *testing.T) {
	b, err := NewJsonBodyFromBytes([]byte(`{"model":"glm-5.2"}`))
	if err != nil {
		t.Fatal(err)
	}

	out, err := io.ReadAll(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"model":"glm-5.2"}` {
		t.Errorf("read = %s", out)
	}
	if _, err := b.Read(make([]byte, 8)); err != io.EOF {
		t.Errorf("post-EOF read err = %v", err)
	}
	if err := b.Close(); err != nil {
		t.Errorf("close = %v", err)
	}
}

func TestReadIsStreamed(t *testing.T) {
	b := NewJsonBody(map[string]any{"k": "vvvvvvvv"})
	buf := make([]byte, 4)
	n, err := b.Read(buf)
	if err != nil || string(buf[:n]) != `{"k"` {
		t.Fatalf("first read = %q %v", buf[:n], err)
	}
	rest, err := io.ReadAll(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := `{"k"` + string(rest); got != `{"k":"vvvvvvvv"}` {
		t.Errorf("streamed body = %s", got)
	}
}

func TestFromBytesRejectsNonObject(t *testing.T) {
	for _, raw := range []string{`not json`, `[1,2]`, `42`, `null`} {
		if _, err := NewJsonBodyFromBytes([]byte(raw)); err == nil {
			t.Errorf("NewJsonBodyFromBytes(%q) should fail", raw)
		}
	}
}

func TestMarshalIsPureSnapshot(t *testing.T) {
	b := NewJsonBody(map[string]any{"k": "vvvv"})

	// Partial read first: 4 of the 11 wire bytes.
	buf := make([]byte, 4)
	n, err := b.Read(buf)
	if err != nil || string(buf[:n]) != `{"k"` {
		t.Fatalf("partial read = %q %v", buf[:n], err)
	}

	// Marshal returns the FULL bytes without disturbing the cursor.
	raw, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"k":"vvvv"}` {
		t.Errorf("Marshal = %s", raw)
	}

	// Read CONTINUES from the cursor, not from the start.
	rest, err := io.ReadAll(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := `{"k"` + string(rest); got != `{"k":"vvvv"}` {
		t.Errorf("read after Marshal = %s, want continuation", got)
	}

	// Marshal is stable: same bytes again.
	raw2, _ := b.Marshal()
	if string(raw2) != string(raw) {
		t.Errorf("Marshal not stable: %s vs %s", raw2, raw)
	}
}

func TestMarshalBeforeAnyRead(t *testing.T) {
	b := NewJsonBody(map[string]any{"a": float64(1)})
	raw, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"a":1}` {
		t.Errorf("Marshal = %s", raw)
	}
	out, err := io.ReadAll(b)
	if err != nil || string(out) != `{"a":1}` {
		t.Errorf("Read after Marshal = %s %v", out, err)
	}
}

func TestSetRequestBodyInstallSemantics(t *testing.T) {
	b, err := NewJsonBodyFromBytes([]byte(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	SetRequestBody(req, b)

	if req.ContentLength != -1 {
		t.Errorf("ContentLength = %d, want -1 (chunked)", req.ContentLength)
	}
	if req.Header.Get("Content-Length") != "" {
		t.Error("Content-Length header must be dropped")
	}
	if req.Body != io.ReadCloser(b) {
		t.Error("req.Body must be the body itself")
	}

	// Each GetBody call serves the full bytes with an independent reader.
	for i := 0; i < 3; i++ {
		rc, err := req.GetBody()
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != `{"model":"m"}` {
			t.Errorf("GetBody #%d = %s", i, out)
		}
	}
}

func TestSetRequestBodyRegularReadStillWorks(t *testing.T) {
	// The body remains a plain io.ReadCloser for handlers that don't know
	// the type: reading drains it exactly once.
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	SetRequestBody(req, NewJsonBody(map[string]any{"a": float64(1)}))
	out, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":1}` {
		t.Errorf("regular read = %s", out)
	}
}

// TestShallowVariantIsolation locks the immutability contract as llm_route
// relies on it: a rewritten-model variant built from a top-level shallow
// copy shares nested values with the original (both stay read-only), and
// installing a NEW body on a request never disturbs another request (or the
// original body object) — the property the old Clone-based design got from
// deep copies.
func TestShallowVariantIsolation(t *testing.T) {
	orig := NewJsonBody(map[string]any{
		"model":    "mc/glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})

	// llm_route's variant: shallow top-level copy with the model rewritten.
	obj := make(map[string]any, len(orig.Obj))
	for k, v := range orig.Obj {
		obj[k] = v
	}
	obj["model"] = "glm-5.2"
	variant := NewJsonBody(obj)

	if variant.Obj["model"] != "glm-5.2" {
		t.Fatalf("variant model = %v", variant.Obj["model"])
	}
	// The original is untouched — this is what makes fallthrough safe.
	if orig.Obj["model"] != "mc/glm-5.2" {
		t.Errorf("original model mutated: %v", orig.Obj["model"])
	}

	// A handler "modifying" the body installs a new one; the request seen
	// by the next handler carries the new value, and a request still
	// holding the variant reads the variant.
	reqA := httptest.NewRequest(http.MethodPost, "/", nil)
	SetRequestBody(reqA, variant)
	reqB := httptest.NewRequest(http.MethodPost, "/", nil)
	SetRequestBody(reqB, NewJsonBody(map[string]any{"model": "replaced"}))
	outA, _ := io.ReadAll(reqA.Body)
	if !strings.Contains(string(outA), `"model":"glm-5.2"`) {
		t.Errorf("reqA body = %s (model must stay glm-5.2)", outA)
	}
}

// TestFromBodyIdempotent: an already-installed body is returned as is.
func TestFromBodyIdempotent(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	orig := NewJsonBody(map[string]any{"x": float64(1)})
	SetRequestBody(req, orig)
	b, err := FromBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if b != orig {
		t.Error("FromBody must return the installed body unchanged")
	}
}

// TestFromBodyErrors keeps the error paths' readability contract.
func TestFromBodyErrors(t *testing.T) {
	t.Run("non-JSON body stays readable", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("not json"))
		if _, err := FromBody(req); err == nil {
			t.Error("non-JSON body should fail")
		}
		out, err := io.ReadAll(req.Body)
		if err != nil || string(out) != "not json" {
			t.Errorf("body must be re-installed readable, got %q %v", out, err)
		}
	})
	t.Run("empty body errors", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(""))
		if _, err := FromBody(req); err == nil {
			t.Error("empty body should fail")
		}
	})
}
