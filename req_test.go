package caddy_llm

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
	b := NewJsonBody([]byte(`{"k":"vvvvvvvv"}`))
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
			t.Errorf("FromBytes(%q) should fail", raw)
		}
	}
}

// TestMarshalCachesStableBytes: repeated Marshal/Read serve identical bytes —
// the cache keeps the Read cursor consistent even after node promotion.
func TestMarshalCachesStableBytes(t *testing.T) {
	b := NewJsonBody([]byte(`{"model":"m","nested":{"x":1}}`))

	// Partial read, then node access promotes the form.
	buf := make([]byte, 4)
	if _, err := b.Read(buf); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.Get("model").String(); !ok {
		t.Fatal("Get(model) failed")
	}

	// Marshal still returns the ORIGINAL bytes (raw cache, cursor intact).
	raw, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"model":"m","nested":{"x":1}}` {
		t.Errorf("Marshal = %s, want original bytes", raw)
	}
	// Read continues from the cursor.
	rest, err := io.ReadAll(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := `{"mo` + string(rest); got != `{"model":"m","nested":{"x":1}}` {
		t.Errorf("read after Marshal = %s", got)
	}
}

// TestVariantBodyEncodesFresh: a With-variant body has no cached bytes; its
// first Marshal encodes the variant (untouched members keep original bytes).
func TestVariantBodyEncodesFresh(t *testing.T) {
	orig := NewJsonBody([]byte(`{"model":"mc/x","keep":{"a":[1,2]}}`))
	variant := &JsonReqBody{LazyJsonNode: *orig.With("model", "glm-5.2")}

	out, err := io.ReadAll(variant)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"model":"glm-5.2"`) {
		t.Errorf("variant body = %s", out)
	}
	if !strings.Contains(string(out), `"keep":{"a":[1,2]}`) {
		t.Errorf("untouched member not inlined verbatim: %s", out)
	}
	// Original untouched.
	if m, _ := orig.Get("model").String(); m != "mc/x" {
		t.Errorf("original mutated: %q", m)
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
	SetRequestBody(req, NewJsonBody([]byte(`{"a":1}`)))
	out, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":1}` {
		t.Errorf("regular read = %s", out)
	}
}

// TestFromBodyIdempotent: an already-installed body is returned as is.
func TestFromBodyIdempotent(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	orig := NewJsonBody([]byte(`{"x":1}`))
	SetRequestBody(req, orig)
	b, err := FromBody(req)
	if err != nil {
		t.Fatal(err)
	}
	if b != orig {
		t.Error("FromBody must return the installed body unchanged")
	}
	// Lazy: node access works through the body.
	if x, ok := b.Get("x").Int(); !ok || x != 1 {
		t.Errorf("Get(x) = %v %v", x, ok)
	}
	if b.Type() != TypeObject {
		t.Errorf("Type = %v", b.Type())
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
