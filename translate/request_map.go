package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// TranslateRequestMap translates an already-parsed Anthropic request body
// into an already-parsed OpenAI request body. It is semantically identical
// to marshal → TranslateRequest → unmarshal, but the output is assembled
// without re-parsing: complex field values are embedded as
// json.RawMessage, which marshals inline when the body bytes are produced.
func TranslateRequestMap(in map[string]any) (map[string]any, error) {
	raw, err := marshalPlain(in)
	if err != nil {
		return nil, fmt.Errorf("encode request body: %w", err)
	}
	var req AnthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}
	out, err := TranslateRequest(&req)
	if err != nil {
		return nil, err
	}
	return openaiRequestMap(out), nil
}

// openaiRequestMap assembles the parsed form of an OpenAIRequest. Scalars
// go in directly; each complex value is marshaled once and embedded as
// json.RawMessage, mirroring the struct's json tags exactly.
func openaiRequestMap(out *OpenAIRequest) map[string]any {
	m := map[string]any{
		"model": out.Model,
	}
	if len(out.Messages) > 0 {
		msgs := make([]any, len(out.Messages))
		for i := range out.Messages {
			msgs[i] = mustRawJSON(out.Messages[i])
		}
		m["messages"] = msgs
	}
	if len(out.Tools) > 0 {
		tools := make([]any, len(out.Tools))
		for i := range out.Tools {
			tools[i] = mustRawJSON(out.Tools[i])
		}
		m["tools"] = tools
	}
	if len(out.ToolChoice) > 0 {
		m["tool_choice"] = out.ToolChoice
	}
	if out.MaxTokens != 0 {
		m["max_tokens"] = out.MaxTokens
	}
	if out.Temperature != nil {
		m["temperature"] = *out.Temperature
	}
	if out.TopP != nil {
		m["top_p"] = *out.TopP
	}
	if len(out.Stop) > 0 {
		m["stop"] = out.Stop
	}
	m["stream"] = out.Stream
	if out.StreamOptions != nil {
		m["stream_options"] = map[string]any{"include_usage": out.StreamOptions.IncludeUsage}
	}
	if out.ReasoningEffort != "" {
		m["reasoning_effort"] = out.ReasoningEffort
	}
	return m
}

// mustRawJSON marshals v; failure is impossible for the wire structs in
// this package (all fields are JSON-representable), so a nil fallback is
// enough.
func mustRawJSON(v any) json.RawMessage {
	b, err := marshalPlain(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// marshalPlain encodes v without HTML escaping. Two reasons it matters
// here: decoding into json.RawMessage fields must keep raw tokens
// byte-identical to a direct unmarshal of client bytes (SetEscapeHTML
// escaping would survive inside RawMessage and double-escape later), and
// outer marshals re-apply their own escaping policy exactly once.
func marshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	return b[:len(b)-1], nil // strip Encode's trailing newline
}
