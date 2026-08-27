package trans

import "encoding/json"

// ---------- OpenAI chat-completions wire types ----------

// OpenAIMessage is one message in the messages array.
type OpenAIMessage struct {
	Role       string           `json:"role"`              // system | user | assistant | tool
	Content    json.RawMessage  `json:"content,omitempty"` // string or []ContentPart
	Reasoning  string           `json:"reasoning_content,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"` // role=tool
	Name       string           `json:"name,omitempty"`
}

// ContentPart is a tagged part in a content array.
type ContentPart struct {
	Type     string    `json:"type"` // text | image_url
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL carries an image reference (http(s) URL or data: URL).
type ImageURL struct {
	URL string `json:"url"`
}

// OpenAIToolCall is an assistant tool invocation.
type OpenAIToolCall struct {
	// Index is used only in streaming deltas (which tool call this belongs to).
	Index    *int           `json:"index,omitempty"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"` // "function"
	Function OpenAIFunction `json:"function"`
}

// OpenAIFunction names a function and carries its arguments as a JSON string
// (in tool calls) or its parameter schema (in tool definitions).
type OpenAIFunction struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Arguments   string `json:"arguments,omitempty"`
	// Parameters is used only in tool definitions (input_schema).
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// OpenAITool is a tool definition.
type OpenAITool struct {
	Type     string         `json:"type"` // "function"
	Function OpenAIFunction `json:"function"`
}

// ---------- OpenAI response types ----------

// OpenAIResponse is a non-streaming chat-completions response.
type OpenAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"` // "chat.completion"
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

// OpenAIChoice is one choice in a response.
type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason *string       `json:"finish_reason"` // stop | length | tool_calls | content_filter
}

// OpenAIUsage is the usage object.
type OpenAIUsage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// PromptTokensDetails breaks down prompt tokens.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// ---------- OpenAI streaming chunk ----------

// OpenAIChunk is one chat.completion.chunk SSE payload.
type OpenAIChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *OpenAIUsage  `json:"usage,omitempty"`
}

// ChunkChoice is one choice in a streaming chunk.
type ChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// ChunkDelta is the incremental payload of a chunk.
type ChunkDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
}
