package trace

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestConcurrentStreamsInterleave: two SSE streams whose Saves interleave in
// ONE file. Each segment's raw_log_idx row claims a CONTIGUOUS byte range
// [offset, offset+size). If A's chunks are separated by B's chunks in the
// file, A's row range contains B's bytes.
func TestConcurrentStreamsInterleave(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Simulate two streaming responses interleaving on the writer goroutines
	// (Save's mutex serializes each chunk, but chunks alternate).
	seq := []struct {
		id  string
		chk string
	}{
		{"A", "A-head\r\n\r\n"},
		{"B", "B-head\r\n\r\n"},
		{"A", "A1-"}, {"B", "B1-"},
		{"A", "A2-"}, {"B", "B2-"},
		{"A", "A3"}, {"B", "B3"},
	}
	for _, st := range seq {
		if err := s.Save(ctx, st.id, "glm", false, []byte(st.chk)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"A", "B"} {
		if err := s.RecordRequest(ctx, id, "glm", time.Now(), 1, 200, 0, 0, nil); err != nil {
			t.Fatal(err)
		}
	}

	for _, id := range []string{"A", "B"} {
		got, err := s.Get(ctx, id, "glm")
		if err != nil {
			t.Fatal(err)
		}
		resp := string(got.Response)
		want := id + "-head\r\n\r\n" + id + "1-" + id + "2-" + id + "3"
		if resp != want {
			t.Errorf("%s's response corrupted:\n got: %q\nwant: %q", id, resp, want)
		}
		_ = strings.TrimSpace
	}
}

// TestGetDuringConcurrentSaves: readers (Get) run concurrently with writers
// (Save) — the RWMutex must let both proceed, and each Get must see only
// its own exchange's bytes.
// chunkUnit is one streaming write in the concurrency tests (19 bytes).
const chunkUnit = "w-chunk-0123456789;"

func TestGetDuringConcurrentSaves(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Seed the first row so concurrent Gets never see an empty exchange.
	if err := s.Save(ctx, "W", "glm", false, []byte("w-chunk-0123456789;")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 199; i++ {
			_ = s.Save(ctx, "W", "glm", false, []byte("w-chunk-0123456789;"))
		}
	}()
	for i := 0; i < 50; i++ {
		got, err := s.Get(ctx, "W", "glm")
		if err != nil {
			t.Fatalf("concurrent Get: %v", err)
		}
		// Every read must be a whole number of chunk units — partial or
		// contaminated reads break the repetition pattern.
		if n := len(got.Response); n%len(chunkUnit) != 0 {
			t.Fatalf("partial chunk read: %d bytes", n)
		}
		if !strings.HasPrefix(string(got.Response), "w-chunk-") {
			t.Fatalf("response start = %q", got.Response[:16])
		}
	}
	<-done

	// Final read must contain exactly the 200 chunks in order.
	got, err := s.Get(ctx, "W", "glm")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat(chunkUnit, 200)
	if string(got.Response) != want {
		t.Errorf("final read = %d bytes, want %d", len(got.Response), len(want))
	}
}
