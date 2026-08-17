// Package translate converts between the Anthropic Messages API wire format
// and the OpenAI Chat Completions wire format. It is a pure library with no
// Caddy dependencies, so it can be unit-tested (and golden-tested against real
// traffic logs) in isolation.
package translate

import "encoding/json"

// ---------- Anthropic /v1/messages wire types ----------

// AnthropicRequest is an Anthropic Messages API request body.
type AnthropicRequest struct {
	Model         string               `json:"model"`
	Messages      []AnthropicMessage   `json:"messages"`
	System        json.RawMessage      `json:"system,omitempty"` // string or []ContentBlock
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	MaxTokens     int                  `json:"max_tokens"`
	Metadata      *AnthropicMetadata   `json:"metadata,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	TopK          int                  `json:"top_k,omitempty"`
	Thinking      *AnthropicThinking   `json:"thinking,omitempty"`

	// OutputConfig carries effort/format knobs for adaptive-thinking models.
	OutputConfig *AnthropicOutputConfig `json:"output_config,omitempty"`
}

// AnthropicOutputConfig is the request output_config field.
type AnthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// AnthropicMessage is one message in the messages array.
type AnthropicMessage struct {
	Role    string          `json:"role"`    // user | assistant
	Content json.RawMessage `json:"content"` // string or []ContentBlock
}

// AnthropicMetadata is the request metadata field.
type AnthropicMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// AnthropicThinking configures extended thinking.
type AnthropicThinking struct {
	Type         string `json:"type"` // enabled | disabled | adaptive | auto
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// AnthropicToolChoice selects tool use behavior.
type AnthropicToolChoice struct {
	Type string `json:"type"` // auto | any | tool
	Name string `json:"name,omitempty"`
}

// AnthropicTool is a tool definition.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ---------- Anthropic content blocks ----------

// ContentBlock is a tagged content block in message content arrays and in
// response `content`.
type ContentBlock struct {
	Type string `json:"type"` // text | image | tool_use | tool_result | thinking | redacted_thinking | document

	// text
	Text string `json:"text,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // string or []ContentBlock
	IsError   *bool           `json:"is_error,omitempty"`

	// image / document
	Source *AnthropicSource `json:"source,omitempty"`
}

// AnthropicSource is an image/document source.
type AnthropicSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ---------- Anthropic response types ----------

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

// ParseContent decodes a message Content field, which may be a plain string or
// an array of content blocks.
func ParseContent(raw json.RawMessage) (blocks []ContentBlock, text string, wasString bool) {
	if len(raw) == 0 {
		return nil, "", false
	}
	if err := json.Unmarshal(raw, &text); err == nil {
		return nil, text, true
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks, "", false
	}
	return nil, "", false
}

// ParseSystem decodes the top-level system field, which may be a plain string
// or an array of content blocks.
func ParseSystem(raw json.RawMessage) []ContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil
		}
		return []ContentBlock{{Type: "text", Text: s}}
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	return nil
}
