package translate

import (
	"encoding/json"
	"net/http"
)

// TranslateResponse converts a non-streaming OpenAI chat-completions response
// into an Anthropic Messages API response. model overrides the reported model
// when non-empty (the request's model name, preferred over the upstream's).
func TranslateResponse(in *OpenAIResponse, model string) *AnthropicResponse {
	out := &AnthropicResponse{
		ID:           in.ID,
		Type:         "message",
		Role:         "assistant",
		Model:        in.Model,
		Content:      []ContentBlock{}, // non-nil: empty means "no content"
		StopSequence: nil,
	}
	if model != "" {
		out.Model = model
	}

	var choice *OpenAIChoice
	if len(in.Choices) > 0 {
		choice = &in.Choices[0]
	}

	var hasToolUse bool
	if choice != nil {
		out.Content = translateMessageContent(&choice.Message)
		for _, b := range out.Content {
			if b.Type == "tool_use" {
				hasToolUse = true
			}
		}
	}

	out.StopReason = "end_turn"
	if choice != nil && choice.FinishReason != nil {
		out.StopReason = mapFinishReason(*choice.FinishReason)
	}
	// finish_reason may disagree with actual content; content wins.
	if hasToolUse {
		out.StopReason = "tool_use"
	} else if out.StopReason == "tool_use" {
		out.StopReason = "end_turn"
	}

	u := in.Usage
	out.Usage = AnthropicUsage{
		InputTokens:          u.PromptTokens - cachedTokens(u),
		CacheReadInputTokens: cachedTokens(u),
		OutputTokens:         u.CompletionTokens,
	}
	return out
}

// translateMessageContent converts an OpenAI assistant message into Claude
// content blocks: reasoning first, then text, then tool_use blocks.
func translateMessageContent(m *OpenAIMessage) []ContentBlock {
	var blocks []ContentBlock

	if r := m.Reasoning; r != "" {
		blocks = append(blocks, ContentBlock{Type: "thinking", Thinking: r})
	}

	switch {
	case len(m.Content) == 0:
		// no text content
	case jsonLooksLikeString(m.Content):
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil && s != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: s})
		}
	default:
		var parts []ContentPart
		if err := json.Unmarshal(m.Content, &parts); err == nil {
			for _, p := range parts {
				if p.Type == "text" && p.Text != "" {
					blocks = append(blocks, ContentBlock{Type: "text", Text: p.Text})
				}
			}
		}
	}

	for _, tc := range m.ToolCalls {
		input := json.RawMessage(parseToolArgs(tc.Function.Arguments))
		blocks = append(blocks, ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}

	if blocks == nil {
		blocks = []ContentBlock{}
	}
	return blocks
}

// parseToolArgs parses a tool-call arguments JSON string into raw JSON,
// falling back to {} when absent or invalid.
func parseToolArgs(args string) []byte {
	if args == "" {
		return []byte("{}")
	}
	var check any
	if err := json.Unmarshal([]byte(args), &check); err != nil {
		return []byte("{}")
	}
	return []byte(args)
}

func jsonLooksLikeString(raw json.RawMessage) bool {
	return len(raw) > 0 && raw[0] == '"'
}

// mapFinishReason maps an OpenAI finish_reason to a Claude stop_reason.
func mapFinishReason(fr string) string {
	switch fr {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func cachedTokens(u OpenAIUsage) int {
	if u.PromptTokensDetails == nil {
		return 0
	}
	c := u.PromptTokensDetails.CachedTokens
	if c > u.PromptTokens {
		return u.PromptTokens // defensive: never report more cache than prompt
	}
	return c
}

// ---------- error translation ----------

// TranslateError converts an upstream HTTP error (status + body, possibly an
// OpenAI JSON error object) into a Claude error response body:
// {"type":"error","error":{"type":...,"message":...}}.
func TranslateError(status int, body []byte) []byte {
	errType := errorTypeFromStatus(status)
	msg := string(body)
	if len(msg) == 0 {
		msg = http.StatusText(status)
		if msg == "" {
			msg = "upstream error"
		}
	}

	// An upstream JSON error body overrides the derived type and message.
	var parsed struct {
		Error *struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		e := parsed.Error
		if e == nil && parsed.Type != "" {
			e = &struct {
				Type    string `json:"type"`
				Code    any    `json:"code"`
				Message string `json:"message"`
			}{Type: parsed.Type, Message: parsed.Message}
		}
		if e != nil {
			if e.Type != "" {
				errType = e.Type
			} else if s, ok := e.Code.(string); ok && s != "" {
				errType = s
			}
			if e.Message != "" {
				msg = e.Message
			}
		}
	}

	out := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	}
	b, _ := json.Marshal(out)
	return b
}

// errorTypeFromStatus derives a Claude error type from an HTTP status code.
func errorTypeFromStatus(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 402:
		return "billing_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 413:
		return "request_too_large"
	case status == 429:
		return "rate_limit_error"
	case status == 504:
		return "timeout_error"
	case status == 529:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}
