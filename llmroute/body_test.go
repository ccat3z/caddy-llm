package llmroute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBodyReadSemantics(t *testing.T) {
	b, err := FromBytes([]byte(`{"model":"glm-5.2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if b.Readonly() {
		t.Error("fresh body must be mutable")
	}

	// Mutations before the first Read are reflected in the bytes.
	b.Obj["model"] = "renamed"
	out, err := io.ReadAll(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"model":"renamed"}` {
		t.Errorf("read = %s", out)
	}

	// After a full read: EOF, and mutations no longer take effect.
	if _, err := b.Read(make([]byte, 8)); err != io.EOF {
		t.Errorf("post-EOF read err = %v", err)
	}
	if !b.Readonly() {
		t.Error("Read must freeze the body")
	}
	b.Obj["model"] = "ignored"
	out2, _ := io.ReadAll(b)
	if string(out2) != "" {
		t.Errorf("frozen body re-read = %s, want empty", out2)
	}

	if err := b.Close(); err != nil {
		t.Errorf("close = %v", err)
	}
}

func TestBodyReadIsStreamed(t *testing.T) {
	b := New(map[string]any{"k": "vvvvvvvv"})
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
		if _, err := FromBytes([]byte(raw)); err == nil {
			t.Errorf("FromBytes(%q) should fail", raw)
		}
	}
}

func TestSetRequestBodyRetrySemantics(t *testing.T) {
	b, err := FromBytes([]byte(`{"model":"m"}`))
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
		t.Error("req.Body must be the Body itself")
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
	// GetBody froze the bytes: mutations are now ineffective (and harmless).
	if !b.Readonly() {
		t.Error("GetBody must freeze the body")
	}
}

func TestSetRequestBodyRegularReadStillWorks(t *testing.T) {
	// The body remains a plain io.ReadCloser for handlers that don't know
	// the type: reading drains it exactly once.
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	SetRequestBody(req, New(map[string]any{"a": float64(1)}))
	out, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":1}` {
		t.Errorf("regular read = %s", out)
	}
}

func TestCloneIsolation(t *testing.T) {
	orig := New(map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "parts": []any{"x"}}},
		"meta":     map[string]any{"nested": map[string]any{"deep": true}},
	})
	clone := orig.Clone()

	clone.Obj["model"] = "changed"
	clone.Obj["messages"].([]any)[0].(map[string]any)["role"] = "assistant"
	clone.Obj["messages"].([]any)[0].(map[string]any)["parts"].([]any)[0] = "y"
	clone.Obj["meta"].(map[string]any)["nested"].(map[string]any)["deep"] = false

	if orig.Obj["model"] != "m" {
		t.Errorf("shallow leak: model = %v", orig.Obj["model"])
	}
	msg := orig.Obj["messages"].([]any)[0].(map[string]any)
	if msg["role"] != "user" || msg["parts"].([]any)[0] != "x" {
		t.Errorf("slice leak: %v", msg)
	}
	if orig.Obj["meta"].(map[string]any)["nested"].(map[string]any)["deep"] != true {
		t.Error("nested map leak")
	}

	// A clone of an already-frozen body is mutable again.
	out, _ := io.ReadAll(orig)
	if len(out) == 0 {
		t.Error("orig read empty")
	}
	if clone.Readonly() {
		t.Error("clone of frozen body must be mutable")
	}
}
