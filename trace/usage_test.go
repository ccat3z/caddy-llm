package trace

import (
	"strings"
	"testing"
)

func TestExtractUsageClaudeJSON(t *testing.T) {
	body := `{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":526,"output_tokens":79,"cache_read_input_tokens":97152,"cache_creation_input_tokens":0}}`
	u := extractUsage([]byte(body))
	if u == nil {
		t.Fatal("no usage")
	}
	if u.Input != 526 || u.Cache != 97152 || u.Output != 79 {
		t.Errorf("usage = %+v", u)
	}
}

func TestExtractUsageOpenAIJSON(t *testing.T) {
	body := `{"id":"cc-1","object":"chat.completion","choices":[],"usage":{"prompt_tokens":24277,"completion_tokens":101,"prompt_tokens_details":{"cached_tokens":1024}}}`
	u := extractUsage([]byte(body))
	if u == nil {
		t.Fatal("no usage")
	}
	if u.Input != 24277 || u.Cache != 1024 || u.Output != 101 {
		t.Errorf("usage = %+v", u)
	}
}

func TestExtractUsageClaudeSSE(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","content":[],"usage":{"input_tokens":526,"cache_creation_input_tokens":0,"cache_read_input_tokens":97152,"output_tokens":0}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"pong"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":526,"output_tokens":79,"cache_read_input_tokens":97152,"cache_creation_input_tokens":0}}`,
		``,
	}, "\n")
	u := extractUsage([]byte(body))
	if u == nil {
		t.Fatal("no usage")
	}
	if u.Input != 526 || u.Cache != 97152 || u.Output != 79 {
		t.Errorf("usage = %+v", u)
	}
}

func TestExtractUsageOpenAISSE(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"cc","object":"chat.completion.chunk","choices":[{"delta":{"content":"pong"}}]}`,
		``,
		`data: {"id":"cc","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":24277,"completion_tokens":101,"prompt_tokens_details":{"cached_tokens":1024}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	u := extractUsage([]byte(body))
	if u == nil {
		t.Fatal("no usage")
	}
	if u.Input != 24277 || u.Cache != 1024 || u.Output != 101 {
		t.Errorf("usage = %+v", u)
	}
}

func TestExtractUsageNone(t *testing.T) {
	for _, body := range []string{
		``,
		`{"type":"error","error":{"type":"api_error","message":"boom"}}`,
		`data: [DONE]`,
		`not json at all`,
	} {
		if u := extractUsage([]byte(body)); u != nil {
			t.Errorf("body %q: want nil, got %+v", body, u)
		}
	}
}

func TestExtractUsageTailWindow(t *testing.T) {
	// A stream larger than the tail window: usage must still be found after
	// the window drops the head (first buffered line is truncated).
	filler := strings.Repeat(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+strings.Repeat("x", 1000)+`"}}`+"\n\n", 80)
	tail := `event: message_delta` + "\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":5,"output_tokens":42}}` + "\n"
	var b tailBuffer
	b.Write([]byte(filler + tail))
	u := extractUsage(b.buf)
	if u == nil {
		t.Fatal("no usage")
	}
	if u.Input != 5 || u.Output != 42 {
		t.Errorf("usage = %+v", u)
	}
}

func TestNormalizeCacheInInput(t *testing.T) {
	u := &Usage{Input: 24277, Cache: 1024, Output: 101}
	n := u.normalize(true)
	if n.Input != 23253 || n.Cache != 1024 || n.Output != 101 {
		t.Errorf("normalized = %+v", n)
	}
	u2 := &Usage{Input: 526, Cache: 97152, Output: 79}
	n2 := u2.normalize(false)
	if n2.Input != 526 || n2.Cache != 97152 {
		t.Errorf("normalized = %+v", n2)
	}
	// Defensive: never go negative.
	u3 := &Usage{Input: 10, Cache: 50, Output: 1}
	if n3 := u3.normalize(true); n3.Input != 0 {
		t.Errorf("negative clamp: %+v", n3)
	}
}

func TestTailBufferBounds(t *testing.T) {
	var b tailBuffer
	b.Write([]byte(strings.Repeat("a", tailSize+100)))
	if len(b.buf) != tailSize {
		t.Fatalf("buf len = %d", len(b.buf))
	}
	b.Write([]byte("bb"))
	if len(b.buf) != tailSize {
		t.Fatalf("buf len = %d", len(b.buf))
	}
}
