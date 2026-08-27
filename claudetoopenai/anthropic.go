// Package translate converts the OpenAI Chat Completions wire format into
// the Anthropic Messages API wire format (the response/stream direction).
// Request translation lives in internal/ This is a pure library with
// no Caddy dependencies.
package claudetoopenai

import "encoding/json"

// ---------- Anthropic /v1/messages response types ----------

// ContentBlock is a tagged content block in message content arrays.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	Source *AnthropicSource `json:"source,omitempty"`
}

// AnthropicSource is an image/document source.
type AnthropicSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// AnthropicResponse is a non-streaming Messages API response.
type AnthropicResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"` // "message"
	Role         string         `json:"role"` // "assistant"
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        AnthropicUsage `json:"usage"`
}

// AnthropicUsage is the usage object in both responses and message_start/delta.
type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens"`
}
