package trace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*diskStore, string) {
	t.Helper()
	dir := t.TempDir()
	d := newDiskStore(filepath.Join(dir, "traces.jsonl"))
	if err := d.open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.close() })
	return d, dir
}

func entry(id, stage string, status int) *Entry {
	return &Entry{
		ID: id, Stage: stage, Timestamp: time.Now().UTC(),
		Method: "POST", Path: "/v1/messages",
		RequestBody: []byte(`{"a":1}`), Status: status,
		ResponseBody: []byte(`{"b":2}`), DurationMS: 42,
	}
}

func TestDiskStoreRoundTrip(t *testing.T) {
	d, _ := newTestStore(t)
	ctx := context.Background()

	if err := d.Append(ctx, entry("t1", "claude", 200)); err != nil {
		t.Fatal(err)
	}
	if err := d.Append(ctx, entry("t2", "openai", 200)); err != nil {
		t.Fatal(err)
	}

	got, err := d.Get(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Stage != "claude" || got.Status != 200 || string(got.RequestBody) != `{"a":1}` {
		t.Errorf("entry = %+v", got)
	}

	// Newest first.
	list, err := d.List(ctx, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "t2" {
		t.Errorf("list = %+v", list)
	}

	// Stage filter.
	list, err = d.List(ctx, Query{Stage: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "t1" {
		t.Errorf("filtered list = %+v", list)
	}

	// Limit/offset.
	list, _ = d.List(ctx, Query{Limit: 1})
	if len(list) != 1 || list[0].ID != "t2" {
		t.Errorf("limited list = %+v", list)
	}

	if _, err := d.Get(ctx, "missing"); err != os.ErrNotExist {
		t.Errorf("missing Get err = %v", err)
	}
}

func TestDiskStoreReindex(t *testing.T) {
	d, dir := newTestStore(t)
	ctx := context.Background()
	if err := d.Append(ctx, entry("persist", "openai", 500)); err != nil {
		t.Fatal(err)
	}
	_ = d.close()

	// Reopen: index rebuilt from disk.
	d2 := newDiskStore(filepath.Join(dir, "traces.jsonl"))
	if err := d2.open(); err != nil {
		t.Fatal(err)
	}
	defer d2.close()
	got, err := d2.Get(ctx, "persist")
	if err != nil {
		t.Fatal(err)
	}
	if got.Stage != "openai" || got.Status != 500 {
		t.Errorf("reopened entry = %+v", got)
	}
	// Append after reopen continues offsets correctly.
	if err := d2.Append(ctx, entry("after", "claude", 200)); err != nil {
		t.Fatal(err)
	}
	list, _ := d2.List(ctx, Query{})
	if len(list) != 2 || list[0].ID != "after" {
		t.Errorf("post-reopen list = %+v", list)
	}
}

func TestTraceAPIServeHTTP(t *testing.T) {
	d, _ := newTestStore(t)
	ctx := context.Background()
	if err := d.Append(ctx, entry("t1", "claude", 200)); err != nil {
		t.Fatal(err)
	}
	api := &TraceAPI{app: &Store{disk: d}}

	// List.
	r := httptest.NewRequest("GET", "/llm/traces", nil)
	w := httptest.NewRecorder()
	if err := api.ServeHTTP(w, r, nopHandler{}); err != nil {
		t.Fatal(err)
	}
	var list []EntrySummary
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body: %v (%s)", err, w.Body)
	}
	if len(list) != 1 || list[0].ID != "t1" || list[0].ReqBytes != 7 {
		t.Errorf("list = %+v", list)
	}

	// Get by id.
	r = httptest.NewRequest("GET", "/llm/traces/t1", nil)
	w = httptest.NewRecorder()
	if err := api.ServeHTTP(w, r, nopHandler{}); err != nil {
		t.Fatal(err)
	}
	var e Entry
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("entry body: %v (%s)", err, w.Body)
	}
	if e.ID != "t1" || string(e.ResponseBody) != `{"b":2}` {
		t.Errorf("entry = %+v", e)
	}

	// Missing id.
	r = httptest.NewRequest("GET", "/llm/traces/nope", nil)
	w = httptest.NewRecorder()
	if err := api.ServeHTTP(w, r, nopHandler{}); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("missing id status = %d", w.Code)
	}

	// Non-GET passes through.
	r = httptest.NewRequest("POST", "/llm/traces", nil)
	w = httptest.NewRecorder()
	var called bool
	if err := api.ServeHTTP(w, r, calledHandler{&called}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("POST should pass through")
	}
}

type nopHandler struct{}

func (nopHandler) ServeHTTP(http.ResponseWriter, *http.Request) error { return nil }

type calledHandler struct{ called *bool }

func (c calledHandler) ServeHTTP(http.ResponseWriter, *http.Request) error {
	*c.called = true
	return nil
}
