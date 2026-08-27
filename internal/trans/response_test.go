package trans

import (
	"encoding/json"
	"testing"
)

func TestTranslateResponseText(t *testing.T) {
	in := &OpenAIResponse{
		ID: "cc-1", Model: "upstream-model",
		Choices: []OpenAIChoice{{
			Message:      OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"hello"`)},
			FinishReason: strPtr("stop"),
		}},
		Usage: OpenAIUsage{PromptTokens: 100, CompletionTokens: 5},
	}
	out := TranslateResponse(in, "client-model")
	if out.Type != "message" || out.Role != "assistant" {
		t.Errorf("envelope = %s/%s", out.Type, out.Role)
	}
	if out.Model != "client-model" {
		t.Errorf("model = %q (request model should win)", out.Model)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" || out.Content[0].Text != "hello" {
		t.Errorf("content = %+v", out.Content)
	}
	if out.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", out.StopReason)
	}
	if out.Usage.InputTokens != 100 || out.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

func TestTranslateResponseToolCalls(t *testing.T) {
	in := &OpenAIResponse{
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role: "assistant",
				ToolCalls: []OpenAIToolCall{{
					ID: "call_1", Type: "function",
					Function: OpenAIFunction{Name: "Bash", Arguments: `{"command":"ls"}`},
				}},
			},
			FinishReason: strPtr("tool_calls"),
		}},
	}
	out := TranslateResponse(in, "")
	if len(out.Content) != 1 {
		t.Fatalf("content = %+v", out.Content)
	}
	b := out.Content[0]
	if b.Type != "tool_use" || b.ID != "call_1" || b.Name != "Bash" {
		t.Errorf("tool_use block = %+v", b)
	}
	if string(b.Input) != `{"command":"ls"}` {
		t.Errorf("input = %s", b.Input)
	}
	if out.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", out.StopReason)
	}
}

func TestTranslateResponseToolCallArgsFallback(t *testing.T) {
	for _, args := range []string{"", "not json", `{"a":1}`} {
		in := &OpenAIResponse{
			Choices: []OpenAIChoice{{
				Message: OpenAIMessage{ToolCalls: []OpenAIToolCall{{
					Function: OpenAIFunction{Name: "f", Arguments: args},
				}}},
			}},
		}
		out := TranslateResponse(in, "")
		want := args
		if want != `{"a":1}` {
			want = "{}"
		}
		if string(out.Content[0].Input) != want {
			t.Errorf("args %q => input %s, want %s", args, out.Content[0].Input, want)
		}
	}
}

func TestTranslateResponseReasoning(t *testing.T) {
	in := &OpenAIResponse{
		Choices: []OpenAIChoice{{
			Message: OpenAIMessage{
				Role:      "assistant",
				Reasoning: "let me think",
				Content:   json.RawMessage(`"answer"`),
			},
			FinishReason: strPtr("stop"),
		}},
	}
	out := TranslateResponse(in, "")
	if len(out.Content) != 2 || out.Content[0].Type != "thinking" || out.Content[1].Type != "text" {
		t.Errorf("content = %+v", out.Content)
	}
}

func TestTranslateResponseFinishReasons(t *testing.T) {
	cases := map[string]string{
		"stop":           "end_turn",
		"length":         "max_tokens",
		"tool_calls":     "tool_use",
		"function_call":  "tool_use",
		"content_filter": "end_turn",
		"weird":          "end_turn",
	}
	for fr, want := range cases {
		in := &OpenAIResponse{
			Choices: []OpenAIChoice{{
				Message:      OpenAIMessage{Content: json.RawMessage(`"x"`)},
				FinishReason: strPtr(fr),
			}},
		}
		if fr == "tool_calls" || fr == "function_call" {
			// text-only content corrects finish_reason to end_turn (covered below)
			continue
		}
		if got := TranslateResponse(in, "").StopReason; got != want {
			t.Errorf("finish_reason %q => %q, want %q", fr, got, want)
		}
	}
	// tool_calls finish_reason WITH tool_use blocks -> tool_use
	inTC := &OpenAIResponse{
		Choices: []OpenAIChoice{{
			Message:      OpenAIMessage{ToolCalls: []OpenAIToolCall{{Function: OpenAIFunction{Name: "f"}}}},
			FinishReason: strPtr("tool_calls"),
		}},
	}
	if got := TranslateResponse(inTC, "").StopReason; got != "tool_use" {
		t.Errorf("tool_calls with blocks => %q, want tool_use", got)
	}
	// tool_calls finish_reason but no tool_use blocks -> end_turn
	in := &OpenAIResponse{
		Choices: []OpenAIChoice{{
			Message:      OpenAIMessage{Content: json.RawMessage(`"just text"`)},
			FinishReason: strPtr("tool_calls"),
		}},
	}
	if got := TranslateResponse(in, "").StopReason; got != "end_turn" {
		t.Errorf("empty tool_calls => %q, want end_turn", got)
	}
	// missing finish_reason
	in2 := &OpenAIResponse{
		Choices: []OpenAIChoice{{Message: OpenAIMessage{Content: json.RawMessage(`"x"`)}}},
	}
	if got := TranslateResponse(in2, "").StopReason; got != "end_turn" {
		t.Errorf("missing finish_reason => %q", got)
	}
}

func TestTranslateResponseUsageCache(t *testing.T) {
	in := &OpenAIResponse{
		Usage: OpenAIUsage{
			PromptTokens:        100,
			CompletionTokens:    7,
			PromptTokensDetails: &PromptTokensDetails{CachedTokens: 40},
		},
	}
	u := TranslateResponse(in, "").Usage
	if u.InputTokens != 60 || u.CacheReadInputTokens != 40 || u.OutputTokens != 7 {
		t.Errorf("usage = %+v", u)
	}
	// cached > prompt (defensive)
	in.Usage.PromptTokensDetails.CachedTokens = 500
	u = TranslateResponse(in, "").Usage
	if u.InputTokens != 0 || u.CacheReadInputTokens != 100 {
		t.Errorf("defensive usage = %+v", u)
	}
}

func TestTranslateResponseEmptyContent(t *testing.T) {
	out := TranslateResponse(&OpenAIResponse{}, "")
	if out.Content == nil {
		t.Error("content must be non-nil empty slice")
	}
	b, _ := json.Marshal(out)
	if string(b) != `{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}` {
		t.Errorf("envelope = %s", b)
	}
}

func TestTranslateError(t *testing.T) {
	// plain status-derived
	b := TranslateError(429, nil)
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	e := got["error"].(map[string]any)
	if e["type"] != "rate_limit_error" {
		t.Errorf("type = %v", e["type"])
	}
	if e["message"] != "Too Many Requests" {
		t.Errorf("message = %v", e["message"])
	}

	// OpenAI JSON error overrides
	b = TranslateError(500, []byte(`{"error":{"type":"invalid_prompt","message":"bad prompt"}}`))
	json.Unmarshal(b, &got)
	e = got["error"].(map[string]any)
	if e["type"] != "invalid_prompt" || e["message"] != "bad prompt" {
		t.Errorf("override = %v", e)
	}

	// code instead of type
	b = TranslateError(502, []byte(`{"error":{"code":"model_overloaded","message":"try later"}}`))
	json.Unmarshal(b, &got)
	e = got["error"].(map[string]any)
	if e["type"] != "model_overloaded" {
		t.Errorf("code fallback = %v", e)
	}

	// status map coverage
	cases := map[int]string{
		401: "authentication_error", 402: "billing_error", 403: "permission_error",
		404: "not_found_error", 413: "request_too_large", 504: "timeout_error",
		529: "overloaded_error", 500: "api_error", 400: "invalid_request_error",
	}
	for status, want := range cases {
		b := TranslateError(status, nil)
		json.Unmarshal(b, &got)
		if got["error"].(map[string]any)["type"] != want {
			t.Errorf("status %d => %v, want %q", status, got["error"], want)
		}
	}
}

func strPtr(s string) *string { return &s }
