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

func TestExtractUsageResponsesJSON(t *testing.T) {
	body := `{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":14979,"input_tokens_details":{"cached_tokens":14949,"cache_write_tokens":27},"output_tokens":6,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":14985}}`
	u := extractUsage([]byte(body))
	if u == nil {
		t.Fatal("no usage")
	}
	if u.Input != 14979 || u.Cache != 14949 || u.Output != 6 {
		t.Errorf("usage = %+v", u)
	}
}

func TestExtractUsageResponsesSSE(t *testing.T) {
	body := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","output":[],"usage":null}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[],"usage":{"input_tokens":14979,"input_tokens_details":{"cached_tokens":14949,"cache_write_tokens":27},"output_tokens":6,"total_tokens":14985}}}`,
		``,
	}, "\n")
	u := extractUsage([]byte(body))
	if u == nil {
		t.Fatal("no usage")
	}
	if u.Input != 14979 || u.Cache != 14949 || u.Output != 6 {
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

func TestUsageScannerLargeTerminalEvent(t *testing.T) {
	// A Responses terminal event embeds the whole response object and can
	// exceed 64KB (observed: a 75KB response.completed event). Usage at its
	// end must still be found, and the event may span Write calls.
	padding := strings.Repeat("x", 100<<10)
	body := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","instructions":"` + padding + `","output":[],"usage":null}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","instructions":"` + padding + `","output":[],"usage":{"input_tokens":16855,"input_tokens_details":{"cached_tokens":16831,"cache_write_tokens":21},"output_tokens":12}}}`,
		``,
	}, "\n")
	var s usageScanner
	for i := 0; i < len(body); i += 4096 {
		end := i + 4096
		if end > len(body) {
			end = len(body)
		}
		s.Write([]byte(body[i:end]))
	}
	u := s.Result()
	if u == nil {
		t.Fatal("no usage")
	}
	if u.Input != 16855 || u.Cache != 16831 || u.Output != 12 {
		t.Errorf("usage = %+v", u)
	}
}

func TestUsageScannerLargeJSONBody(t *testing.T) {
	// A buffered JSON body larger than the old 64KB tail must still parse.
	body := `{"id":"m1","object":"response","status":"completed","instructions":"` +
		strings.Repeat("x", 100<<10) +
		`","usage":{"input_tokens":14979,"input_tokens_details":{"cached_tokens":14949},"output_tokens":6}}`
	var s usageScanner
	for i := 0; i < len(body); i += 4096 {
		end := i + 4096
		if end > len(body) {
			end = len(body)
		}
		s.Write([]byte(body[i:end]))
	}
	u := s.Result()
	if u == nil {
		t.Fatal("no usage")
	}
	if u.Input != 14979 || u.Cache != 14949 || u.Output != 6 {
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
