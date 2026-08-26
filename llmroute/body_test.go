package llmroute

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestDeepCopyObjIsolation(t *testing.T) {
	orig := map[string]any{
		"model":    "m",
		"messages": []any{map[string]any{"role": "user", "parts": []any{"x"}}},
		"meta":     map[string]any{"nested": map[string]any{"deep": true}},
	}
	clone := deepCopyObj(orig)

	clone["model"] = "changed"
	clone["messages"].([]any)[0].(map[string]any)["role"] = "assistant"
	clone["messages"].([]any)[0].(map[string]any)["parts"].([]any)[0] = "y"
	clone["meta"].(map[string]any)["nested"].(map[string]any)["deep"] = false

	if orig["model"] != "m" {
		t.Errorf("shallow leak: model = %v", orig["model"])
	}
	msg := orig["messages"].([]any)[0].(map[string]any)
	if msg["role"] != "user" || msg["parts"].([]any)[0] != "x" {
		t.Errorf("slice leak: %v", msg)
	}
	if orig["meta"].(map[string]any)["nested"].(map[string]any)["deep"] != true {
		t.Error("nested map leak")
	}
}

func TestFromBody(t *testing.T) {
	t.Run("plain body is parsed and installed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"model":"m","n":1}`))
		b, err := FromBody(req)
		if err != nil {
			t.Fatal(err)
		}
		if req.Body != io.ReadCloser(b) {
			t.Error("FromBody must install the Body on the request")
		}
		if req.ContentLength != -1 {
			t.Errorf("ContentLength = %d, want -1", req.ContentLength)
		}
		if b.Obj["model"] != "m" {
			t.Errorf("obj = %v", b.Obj)
		}
		out, _ := io.ReadAll(req.Body)
		if string(out) != `{"model":"m","n":1}` {
			t.Errorf("read = %s", out)
		}
	})

	t.Run("existing Body returned as is", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		orig := New(map[string]any{"a": float64(1)})
		SetRequestBody(req, orig)
		b, err := FromBody(req)
		if err != nil {
			t.Fatal(err)
		}
		if b != orig {
			t.Error("FromBody must return the installed Body unchanged")
		}
	})

	t.Run("non-JSON body errors and stays readable", func(t *testing.T) {
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
