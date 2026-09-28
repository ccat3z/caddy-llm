package trace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*rawStore, string) {
	t.Helper()
	dir := t.TempDir()
	s := newRawStore(dir)
	if err := s.open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.close() })
	return s, dir
}

// reqMsg builds a minimal replayable request message.
func reqMsg(body string) []byte {
	return []byte("POST /v1/messages HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n\r\n" + body)
}

// respHead builds a response status line + headers + blank line.
func respHead(status int) []byte {
	return []byte("HTTP/1.1 " + itoa(status) + " X\r\nContent-Type: text/event-stream\r\n\r\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestRawStoreRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Two exchanges; one streaming (multiple Saves).
	if err := s.Save(ctx, "t1", "claude", true, reqMsg(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "t1", "claude", false, respHead(200)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "t1", "claude", false, []byte("event: a\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "t1", "claude", false, []byte("event: b\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRequest(ctx, "t1", "claude", time.Now(), 42, 200, len(`{"a":1}`), 30, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "t2", "openai", true, reqMsg(`{"b":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRequest(ctx, "t2", "openai", time.Now(), 7, 200, len(`{"b":2}`), 0, nil); err != nil {
		t.Fatal(err)
	}

	// Get: assembled replayable messages, both directions.
	got, err := s.Get(ctx, "t1", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got.Request), "POST /v1/messages HTTP/1.1\r\n") {
		t.Errorf("request head = %q", got.Request[:40])
	}
	if !strings.HasSuffix(string(got.Request), `{"a":1}`) {
		t.Errorf("request body = %q", got.Request)
	}
	if !strings.HasPrefix(string(got.Response), "HTTP/1.1 200 X\r\n") {
		t.Errorf("response head = %q", got.Response[:20])
	}
	if !strings.HasSuffix(string(got.Response), "event: a\nevent: b\n") {
		t.Errorf("streamed chunks lost: %q", got.Response)
	}
	if got.Status != 200 || got.ReqBytes != len(`{"a":1}`) || got.RespBytes != 30 {
		t.Errorf("detail = %+v", got.RequestSummary)
	}

	// List: newest first.
	list, err := s.List(ctx, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].TraceID != "t2" {
		t.Errorf("list = %+v", list)
	}

	// Trace-name filter.
	list, err = s.List(ctx, Query{TraceName: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].TraceID != "t1" {
		t.Errorf("filtered list = %+v", list)
	}

	// Missing id.
	if _, err := s.Get(ctx, "nope", "claude"); err != os.ErrNotExist {
		t.Errorf("missing = %v, want ErrNotExist", err)
	}
}

// TestRawStoreIncomplete: raw segments without a RecordRequest row are still
// retrievable (crash mid-stream keeps what arrived).
func TestRawStoreIncomplete(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.Save(ctx, "t1", "glm", false, respHead(200)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "t1", "glm", false, []byte("event: one\n")); err != nil {
		t.Fatal(err)
	}
	// no RecordRequest — simulated crash

	got, err := s.Get(ctx, "t1", "glm")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Response), "event: one\n") {
		t.Errorf("incomplete stream bytes = %q", got.Response)
	}
	if got.Status != 0 {
		t.Errorf("status should be zero-valued, got %d", got.Status)
	}

	// But it does not appear in the list.
	list, _ := s.List(ctx, Query{})
	if len(list) != 0 {
		t.Errorf("incomplete exchange listed: %+v", list)
	}
}

// TestRawStoreRotate: crossing the size threshold seals the current segment
// and continues in a new file; Get stitches both parts.
func TestRawStoreRotate(t *testing.T) {
	s, dir := newTestStore(t)
	ctx := context.Background()

	// Shrink the threshold for the test.
	s.mu.Lock()
	firstFile := s.activeName
	s.mu.Unlock()

	// Write chunks big enough that the third Save triggers rotation
	// (rotation happens at Save time when the file already exceeds the
	// threshold).
	big := strings.Repeat("x", rotateSize+1)
	if err := s.Save(ctx, "t1", "glm", false, respHead(200)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "t1", "glm", false, []byte(big)); err != nil {
		t.Fatal(err)
	}
	// This Save lands in a fresh file (previous one exceeded the threshold).
	if err := s.Save(ctx, "t1", "glm", false, []byte("tail")); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	secondFile := s.activeName
	s.mu.Unlock()
	if secondFile == firstFile {
		t.Fatalf("no rotation: still %s", firstFile)
	}
	if err := s.RecordRequest(ctx, "t1", "glm", time.Now(), 1, 200, 0, len(big)+len("tail"), nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "t1", "glm")
	if err != nil {
		t.Fatal(err)
	}
	want := string(respHead(200)) + big + "tail"
	if string(got.Response) != want {
		t.Errorf("rotated stitch: got %d bytes, want %d", len(got.Response), len(want))
	}

	// Two history files on disk.
	entries, _ := filepath.Glob(filepath.Join(dir, "history-*.raw"))
	if len(entries) < 2 {
		t.Errorf("expected >=2 history files, got %v", entries)
	}
}

// TestRawStoreReopen: a new store instance over the same dir sees old data
// via the SQLite index (fresh active file, old files read-only).
func TestRawStoreReopen(t *testing.T) {
	dir := t.TempDir()
	s := newRawStore(dir)
	if err := s.open(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Save(ctx, "t1", "claude", true, reqMsg(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "t1", "claude", false, respHead(200)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "t1", "claude", false, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRequest(ctx, "t1", "claude", time.Now(), 5, 200, 7, 5, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}

	s2 := newRawStore(dir)
	if err := s2.open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.close() })

	list, err := s2.List(ctx, Query{})
	if err != nil || len(list) != 1 {
		t.Fatalf("reopen list = %v err=%v", list, err)
	}
	got, err := s2.Get(ctx, "t1", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got.Response), "hello") {
		t.Errorf("reopen response = %q", got.Response)
	}
}

func TestTraceAPIServeHTTP(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.Save(ctx, "t1", "claude", true, reqMsg(`abcdefg`)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRequest(ctx, "t1", "claude", time.Now(), 3, 200, 7, 0, nil); err != nil {
		t.Fatal(err)
	}
	api := &TraceAPI{app: &Store{db: s}}

	// The route prefix is stripped upstream; the handler sees "" or ids.
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil), nopHandler{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"trace_id":"t1"`) {
		t.Errorf("list = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/t1/claude", nil), nopHandler{})
	var detail struct {
		RequestRaw []byte `json:"request_raw"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("get decode: %v (%s)", err, rec.Body.String())
	}
	if rec.Code != 200 || !strings.Contains(string(detail.RequestRaw), "abcdefg") {
		t.Errorf("get = %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope/claude", nil), nopHandler{})
	if rec.Code != 404 {
		t.Errorf("missing = %d", rec.Code)
	}

	// Non-GET passes through.
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil), nopHandler{})
	if rec.Code != 200 {
		t.Errorf("POST should pass through, got %d %q", rec.Code, rec.Body.String())
	}
}

type nopHandler struct{}

func (nopHandler) ServeHTTP(_ http.ResponseWriter, _ *http.Request) error { return nil }

// TestRawStoreRotateConcurrentSegments locks the rotation-seal fix: when a
// rotation fires, EVERY open segment must be sealed — another exchange's
// continuing segment must not keep growing its old-file row while its bytes
// land in the new file (that leaked other requests' bytes on Get).
func TestRawStoreRotateConcurrentSegments(t *testing.T) {
	s, dir := newTestStore(t)
	ctx := context.Background()

	// Two exchanges stream concurrently: A writes a big chunk, B has an open
	// segment in the same file.
	big := strings.Repeat("a", rotateSize+1)
	if err := s.Save(ctx, "A", "glm", false, respHead(200)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "B", "glm", false, respHead(200)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "B", "glm", false, []byte("B1")); err != nil {
		t.Fatal(err)
	}
	// A's write crosses the threshold; B's next Save must trigger rotation
	// and open a FRESH row for B (not grow B's old-file row).
	if err := s.Save(ctx, "A", "glm", false, []byte(big)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, "B", "glm", false, []byte("B2")); err != nil {
		t.Fatal(err)
	}
	// A third exchange writes after rotation; its bytes must not be readable
	// as part of B's trace.
	if err := s.Save(ctx, "C", "glm", false, []byte("C-SECRET")); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRequest(ctx, "B", "glm", time.Now(), 1, 200, 0, 0, nil); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "B", "glm")
	if err != nil {
		t.Fatal(err)
	}
	resp := string(got.Response)
	if resp != string(respHead(200))+"B1B2" {
		t.Errorf("B's response corrupted across rotation: %q (len %d)", truncStrFor(resp, 80), len(resp))
	}
	_ = dir
}

func truncStrFor(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// TestUsageColumns: usage lands in llm_requests, NULL when absent, and the
// series aggregates into interval buckets.
func TestUsageColumns(t *testing.T) {
	s, _ := newTestStore(t)
	defer s.close()
	ctx := context.Background()

	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	recs := []struct {
		id, name string
		ts        time.Time
		usage     *Usage
	}{
		{"a", "mcli", base.Add(5 * time.Minute), &Usage{Input: 100, Cache: 900, Output: 10}},
		{"b", "mcli", base.Add(40 * time.Minute), &Usage{Input: 200, Cache: 800, Output: 20}},
		{"c", "friday", base.Add(70 * time.Minute), &Usage{Input: 300, Cache: 700, Output: 30}},
		{"d", "mcli", base.Add(80 * time.Minute), nil}, // probe, no usage
	}
	for _, r := range recs {
		if err := s.RecordRequest(ctx, r.id, r.name, r.ts, 1, 200, 0, 0, r.usage); err != nil {
			t.Fatal(err)
		}
	}

	// List returns token fields; the probe row has none.
	list, err := s.List(ctx, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("list = %d entries", len(list))
	}
	var probe *RequestSummary
	for i := range list {
		if list[i].TraceID == "a" && (list[i].InputTokens != 100 || list[i].CacheTokens != 900 || list[i].OutputTokens != 10 || !list[i].HasUsage) {
			t.Errorf("entry a = %+v", list[i])
		}
		if list[i].TraceID == "d" {
			probe = &list[i]
		}
	}
	if probe == nil || probe.HasUsage {
		t.Errorf("probe should have no usage: %+v", probe)
	}

	// Hourly buckets: 10:00 has a+b, 11:00 has c+d.
	buckets, err := s.UsageSeries(ctx, 3600, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("buckets = %+v", buckets)
	}
	if buckets[0].Timestamp.Hour() != 10 || buckets[0].Input != 300 || buckets[0].Cache != 1700 || buckets[0].Output != 30 || buckets[0].Requests != 2 {
		t.Errorf("bucket[0] = %+v", buckets[0])
	}
	if buckets[1].Timestamp.Hour() != 11 || buckets[1].Input != 300 || buckets[1].Requests != 2 {
		t.Errorf("bucket[1] = %+v", buckets[1])
	}

	// 30-minute buckets split a/b; trace-name filter applies.
	half, err := s.UsageSeries(ctx, 1800, time.Time{}, time.Time{}, "mcli")
	if err != nil {
		t.Fatal(err)
	}
	if len(half) != 3 {
		t.Fatalf("30m buckets = %+v", half)
	}
	if half[0].Input != 100 || half[1].Input != 200 {
		t.Errorf("30m buckets = %+v", half)
	}

	// Time range filter.
	ranged, err := s.UsageSeries(ctx, 3600, base.Add(50*time.Minute), time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ranged) != 1 || ranged[0].Timestamp.Hour() != 11 {
		t.Errorf("ranged = %+v", ranged)
	}
}
