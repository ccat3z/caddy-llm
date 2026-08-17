package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TranslateRequest converts an Anthropic Messages API request body into an
// OpenAI chat-completions request body.
func TranslateRequest(in *AnthropicRequest) (*OpenAIRequest, error) {
	out := &OpenAIRequest{
		Model:     in.Model,
		MaxTokens: in.MaxTokens,
		Stream:    in.Stream,
	}

	// temperature and top_p are mutually exclusive; temperature wins.
	if in.Temperature != nil {
		t := *in.Temperature
		out.Temperature = &t
	} else if in.TopP != nil {
		p := *in.TopP
		out.TopP = &p
	}

	if len(in.StopSequences) > 0 {
		if len(in.StopSequences) == 1 {
			b, _ := json.Marshal(in.StopSequences[0])
			out.Stop = b
		} else {
			b, _ := json.Marshal(in.StopSequences)
			out.Stop = b
		}
	}

	if eff, ok := reasoningEffort(in); ok {
		out.ReasoningEffort = eff
	}

	if sys := ParseSystem(in.System); len(sys) > 0 {
		msg, err := systemMessage(sys)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, *msg)
	}

	for i := range in.Messages {
		msgs, err := translateMessage(&in.Messages[i])
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out.Messages = append(out.Messages, msgs...)
	}

	for i := range in.Tools {
		t := &in.Tools[i]
		schema := normalizeSchema(t.InputSchema)
		out.Tools = append(out.Tools, OpenAITool{
			Type: "function",
			Function: OpenAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}

	if in.ToolChoice != nil {
		switch in.ToolChoice.Type {
		case "any":
			out.ToolChoice = json.RawMessage(`"required"`)
		case "tool":
			b, _ := json.Marshal(map[string]any{
				"type":     "function",
				"function": map[string]string{"name": in.ToolChoice.Name},
			})
			out.ToolChoice = b
		default: // "auto" and unknown
			out.ToolChoice = json.RawMessage(`"auto"`)
		}
	}

	if in.Stream {
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	return out, nil
}

// reasoningEffort maps the Anthropic thinking config (and output_config for
// adaptive-thinking requests) to an OpenAI reasoning_effort value.
func reasoningEffort(in *AnthropicRequest) (string, bool) {
	t := in.Thinking
	if t == nil {
		return "", false
	}
	switch t.Type {
	case "enabled":
		switch {
		case t.BudgetTokens <= 512:
			return "minimal", true
		case t.BudgetTokens <= 1024:
			return "low", true
		case t.BudgetTokens <= 8192:
			return "medium", true
		case t.BudgetTokens <= 24576:
			return "high", true
		default:
			return "xhigh", true
		}
	case "adaptive", "auto":
		if in.OutputConfig != nil && in.OutputConfig.Effort != "" {
			return strings.ToLower(in.OutputConfig.Effort), true
		}
		return "xhigh", true
	case "disabled":
		return "none", true
	}
	return "", false
}

// systemMessage converts Anthropic system blocks into an OpenAI system message.
func systemMessage(blocks []ContentBlock) (*OpenAIMessage, error) {
	parts := make([]ContentPart, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "text" || strings.TrimSpace(b.Text) == "" {
			continue
		}
		parts = append(parts, ContentPart{Type: "text", Text: b.Text})
	}
	if len(parts) == 0 {
		return nil, nil
	}
	content, err := json.Marshal(parts)
	if err != nil {
		return nil, err
	}
	return &OpenAIMessage{Role: "system", Content: content}, nil
}

// translateMessage converts one Anthropic message into one or more OpenAI
// messages. tool_result blocks become standalone role:"tool" messages emitted
// before the message's own content, so they immediately follow the assistant
// tool_calls message as OpenAI requires. A system-role message mid-conversation
// (Claude Code injects these) becomes a user message wrapped in
// <system-reminder> tags to prevent prompt injection via role confusion.
func translateMessage(m *AnthropicMessage) ([]OpenAIMessage, error) {
	blocks, text, wasString := ParseContent(m.Content)

	if m.Role == "system" {
		if !wasString && len(blocks) > 0 {
			var texts []string
			for _, b := range blocks {
				if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
					texts = append(texts, b.Text)
				}
			}
			text = strings.Join(texts, "\n\n")
		}
		content, err := json.Marshal([]ContentPart{{
			Type: "text",
			Text: "<system-reminder>\n" + text + "\n</system-reminder>",
		}})
		if err != nil {
			return nil, err
		}
		return []OpenAIMessage{{Role: "user", Content: content}}, nil
	}

	var out []OpenAIMessage

	if wasString {
		out = append(out, OpenAIMessage{Role: m.Role, Content: json.RawMessage(jsonString(text))})
		return out, nil
	}

	// First pass: tool_result blocks -> role:"tool" messages.
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		content, err := toolResultContent(b.Content)
		if err != nil {
			return nil, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
		}
		out = append(out, OpenAIMessage{
			Role:       "tool",
			ToolCallID: b.ToolUseID,
			Content:    content,
		})
	}

	// Second pass: the message's own content.
	own := OpenAIMessage{Role: m.Role}
	var parts []ContentPart
	if m.Role == "assistant" {
		var (
			texts     []string
			hasText   bool
			thinkings []string
			toolCalls []OpenAIToolCall
		)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				texts = append(texts, b.Text)
				hasText = true
			case "thinking":
				// Only round-trip signed thinking; redacted thinking never crosses.
				if b.Signature == "" {
					continue
				}
				thinkings = append(thinkings, b.Thinking)
			case "redacted_thinking":
				// dropped
			case "tool_use":
				args := "{}"
				if len(b.Input) > 0 {
					args = string(b.Input)
				}
				toolCalls = append(toolCalls, OpenAIToolCall{
					ID:   b.ID,
					Type: "function",
					Function: OpenAIFunction{
						Name:      b.Name,
						Arguments: args,
					},
				})
			case "image", "document":
				part, err := contentPartFromBlock(&b)
				if err != nil {
					return nil, err
				}
				parts = append(parts, *part)
			default:
				// Unknown block types are dropped.
			}
		}
		if len(thinkings) > 0 {
			own.Reasoning = strings.Join(thinkings, "\n\n")
		}
		if hasText {
			parts = append([]ContentPart{{Type: "text", Text: strings.Join(texts, "\n\n")}}, parts...)
		}
		if len(toolCalls) > 0 {
			own.ToolCalls = toolCalls
		}
	} else {
		for i := range blocks {
			b := &blocks[i]
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				parts = append(parts, ContentPart{Type: "text", Text: b.Text})
			case "image", "document":
				part, err := contentPartFromBlock(b)
				if err != nil {
					return nil, err
				}
				parts = append(parts, *part)
			default:
				// tool_result handled above; unknown types dropped.
			}
		}
	}

	if len(parts) > 0 {
		content, err := json.Marshal(parts)
		if err != nil {
			return nil, err
		}
		own.Content = content
	}

	// Keep the message even if it only carries tool_calls or reasoning; a
	// content-less assistant message serializes content as "".
	if len(own.Content) > 0 || len(own.ToolCalls) > 0 || own.Reasoning != "" {
		if len(own.Content) == 0 {
			own.Content = json.RawMessage(`""`)
		}
		out = append(out, own)
	}
	return out, nil
}

// contentPartFromBlock converts an image or document block into an OpenAI
// image_url content part.
func contentPartFromBlock(b *ContentBlock) (*ContentPart, error) {
	if b.Source == nil {
		return nil, fmt.Errorf("%s block missing source", b.Type)
	}
	switch b.Source.Type {
	case "base64":
		media := b.Source.MediaType
		if media == "" {
			media = "application/octet-stream"
		}
		return &ContentPart{
			Type:     "image_url",
			ImageURL: &ImageURL{URL: "data:" + media + ";base64," + b.Source.Data},
		}, nil
	case "url":
		return &ContentPart{
			Type:     "image_url",
			ImageURL: &ImageURL{URL: b.Source.URL},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported source type %q", b.Source.Type)
	}
}

// toolResultContent converts a tool_result block's content field (string or
// block array) into an OpenAI tool-message content value. Text-only arrays are
// joined into a plain string; arrays containing images stay as parts.
func toolResultContent(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`""`), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return json.RawMessage(jsonString(s)), nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("parse content: %w", err)
	}
	var (
		texts  []string
		parts  []ContentPart
		hasImg bool
	)
	for i := range blocks {
		b := &blocks[i]
		switch b.Type {
		case "text":
			if hasImg {
				parts = append(parts, ContentPart{Type: "text", Text: b.Text})
			} else {
				texts = append(texts, b.Text)
			}
		case "image":
			part, err := contentPartFromBlock(b)
			if err != nil {
				return nil, err
			}
			if !hasImg {
				// Promote accumulated text to parts.
				for _, t := range texts {
					parts = append(parts, ContentPart{Type: "text", Text: t})
				}
				texts = nil
				hasImg = true
			}
			parts = append(parts, *part)
		default:
			// dropped
		}
	}
	if !hasImg {
		return json.RawMessage(jsonString(strings.Join(texts, "\n\n"))), nil
	}
	b, err := json.Marshal(parts)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// jsonString marshals s as a JSON string literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// normalizeSchema recursively ensures every object-type node in a JSON schema
// carries a "properties" key (as an empty object when absent). Some upstreams
// reject object schemas without it.
func normalizeSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out := normalizeSchemaValue(v)
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
}

func normalizeSchemaValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t["type"] == "object" {
			if _, ok := t["properties"].(map[string]any); !ok && t["properties"] == nil {
				if t == nil {
					t = map[string]any{}
				}
				t["properties"] = map[string]any{}
			}
		}
		for k, child := range t {
			t[k] = normalizeSchemaValue(child)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = normalizeSchemaValue(child)
		}
		return t
	default:
		return v
	}
}
