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
		{"A", "A3"},  {"B", "B3"},
	}
	for _, st := range seq {
		if err := s.Save(ctx, st.id, "glm", false, []byte(st.chk)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"A", "B"} {
		if err := s.RecordRequest(ctx, id, "glm", time.Now(), 1, 200, 0, 0); err != nil {
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
