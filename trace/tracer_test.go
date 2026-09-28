package trace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// TestTracerRecordsOnAbortPanic: reverse_proxy panics with
// http.ErrAbortHandler when a response stream breaks mid-way (client
// disconnect mid-SSE). The tracer must still record the exchange — the
// record happens in a defer, so the panic unwinding runs it.
func TestTracerRecordsOnAbortPanic(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.close()
	tr := &Tracer{TraceName: "stub", app: &Store{db: s}}

	downstream := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: partial-chunk\n\n"))
		panic(http.ErrAbortHandler)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"a":1}`))
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if rv := recover(); rv != http.ErrAbortHandler {
				t.Fatalf("panic = %v, want ErrAbortHandler", rv)
			}
		}()
		_ = tr.ServeHTTP(rec, req, downstream)
	}()

	// The aggregate row exists despite the panic.
	list, err := s.List(context.Background(), Query{})
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	e := list[0]
	if e.TraceName != "stub" || e.Status != 200 {
		t.Errorf("entry = %+v", e)
	}

	// And the partial stream bytes are retrievable.
	got, err := s.Get(context.Background(), e.TraceID, "stub")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Response), "partial-chunk") {
		t.Errorf("partial response lost: %q", got.Response)
	}
	if !strings.Contains(string(got.Request), `{"a":1}`) {
		t.Errorf("request lost: %q", got.Request)
	}
}

// TestTracerNormalCompletionStillRecordsOnce: the defer must not
// double-record on the normal path.
func TestTracerNormalCompletionStillRecordsOnce(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.close()
	tr := &Tracer{TraceName: "stub", app: &Store{db: s}}

	downstream := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		_, _ = w.Write([]byte("ok"))
		return nil
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if err := tr.ServeHTTP(httptest.NewRecorder(), req, downstream); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(context.Background(), Query{})
	if len(list) != 1 {
		t.Fatalf("rows = %d, want exactly 1", len(list))
	}
}
