package claudetoopenai

import (
	"encoding/json"
	"fmt"
	"strings"

	caddyllm "github.com/ccat3z/caddy-llm"
)

// TranslateRequest converts an Anthropic Messages API request body into an
// OpenAI chat-completions request body. The input is a lazily-parsed node:
// only the parts the translation reads get parsed. The output is built as Go
// values; untouched input subtrees that pass through verbatim (tool-use
// arguments, normalized schemas) carry their original bytes as RawMessage.
func TranslateRequest(in *caddyllm.LazyJsonNode) (*caddyllm.LazyJsonNode, error) {
	members, ok := in.Obj()
	if !ok {
		return nil, fmt.Errorf("request body is not a JSON object")
	}
	out := map[string]any{}

	// Scalars pass through; integral numbers keep their integer form.
	if v, ok := members["model"]; ok {
		if s, ok := v.String(); ok {
			out["model"] = s
		}
	}
	if v, ok := members["max_tokens"]; ok {
		if n, ok := v.Int(); ok {
			out["max_tokens"] = n
		}
	}
	// stream is always present on the wire (the old struct path emitted the
	// zero value when absent).
	stream := false
	if v, ok := members["stream"]; ok {
		if b, ok := v.Bool(); ok {
			stream = b
		}
	}
	out["stream"] = stream

	// temperature and top_p are mutually exclusive; temperature wins. A JSON
	// null temperature counts as absent.
	if v, ok := members["temperature"]; ok && isPresent(v) {
		if f, ok := v.Float64(); ok {
			out["temperature"] = numValue(f)
		}
	} else if v, ok := members["top_p"]; ok && isPresent(v) {
		if f, ok := v.Float64(); ok {
			out["top_p"] = numValue(f)
		}
	}

	// Stop sequences: one value becomes a bare string, more become an array.
	if v, ok := members["stop_sequences"]; ok {
		if items, ok := v.List(); ok && len(items) > 0 {
			if len(items) == 1 {
				if s, ok := items[0].String(); ok {
					out["stop"] = s
				}
			} else {
				stops := make([]any, 0, len(items))
				for _, it := range items {
					s, ok := it.String()
					if !ok {
						return nil, fmt.Errorf("stop_sequences: element is not a string")
					}
					stops = append(stops, s)
				}
				out["stop"] = stops
			}
		}
	}

	if eff, ok := reasoningEffort(members); ok {
		out["reasoning_effort"] = eff
	}

	var msgs []any
	if sys := systemMessage(members["system"]); sys != nil {
		msgs = append(msgs, sys)
	}
	if v, ok := members["messages"]; ok {
		items, ok := v.List()
		if !ok && v.Type() == caddyllm.TypeObject {
			return nil, fmt.Errorf("messages is not an array")
		}
		for i, item := range items {
			converted, err := translateMessage(item)
			if err != nil {
				return nil, fmt.Errorf("messages[%d]: %w", i, err)
			}
			msgs = append(msgs, converted...)
		}
	}
	if len(msgs) > 0 {
		out["messages"] = msgs
	}

	if v, ok := members["tools"]; ok {
		if items, ok := v.List(); ok && len(items) > 0 {
			tools := make([]any, 0, len(items))
			for i, t := range items {
				tool, err := translateTool(t)
				if err != nil {
					return nil, fmt.Errorf("tools[%d]: %w", i, err)
				}
				tools = append(tools, tool)
			}
			out["tools"] = tools
		}
	}

	if v, ok := members["tool_choice"]; ok {
		tc, err := translateToolChoice(v)
		if err != nil {
			return nil, err
		}
		out["tool_choice"] = tc
	}

	if stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}

	return &caddyllm.LazyJsonNode{Val: out}, nil
}

// isPresent filters absent (missing member / NotExist) and JSON-null nodes.
func isPresent(n *caddyllm.LazyJsonNode) bool {
	return n != nil && n.Type() != caddyllm.TypeNull && n.Type() != caddyllm.TypeNotExist
}

// numValue keeps integral floats as ints so the wire form stays 2000000
// rather than 2e+06.
func numValue(f float64) any {
	if f == float64(int64(f)) && f < 1e15 && f > -1e15 {
		return int64(f)
	}
	return f
}

// reasoningEffort maps the Anthropic thinking config (and output_config for
// adaptive-thinking requests) to an OpenAI reasoning_effort value.
func reasoningEffort(members map[string]*caddyllm.LazyJsonNode) (string, bool) {
	th := members["thinking"]
	if !isPresent(th) {
		return "", false
	}
	tt, _ := th.Get("type").String()
	switch tt {
	case "enabled":
		budget, _ := th.Get("budget_tokens").Int()
		switch {
		case budget <= 512:
			return "minimal", true
		case budget <= 1024:
			return "low", true
		case budget <= 8192:
			return "medium", true
		case budget <= 24576:
			return "high", true
		default:
			return "xhigh", true
		}
	case "adaptive", "auto":
		if oc := members["output_config"]; oc != nil {
			if eff, ok := oc.Get("effort").String(); ok && eff != "" {
				return strings.ToLower(eff), true
			}
		}
		return "xhigh", true
	case "disabled":
		return "none", true
	}
	return "", false
}

// systemMessage converts the Anthropic system prompt (string or block array)
// into an OpenAI system message; whitespace-only or non-text content
// produces none.
func systemMessage(n *caddyllm.LazyJsonNode) map[string]any {
	if n == nil {
		return nil
	}
	var parts []any
	switch n.Type() {
	case caddyllm.TypeString:
		s, _ := n.String()
		if strings.TrimSpace(s) == "" {
			return nil
		}
		parts = append(parts, textPart(s))
	case caddyllm.TypeArray:
		if items, ok := n.List(); ok {
			for _, b := range items {
				if blockType(b) != "text" {
					continue
				}
				t, _ := b.Get("text").String()
				if strings.TrimSpace(t) == "" {
					continue
				}
				parts = append(parts, textPart(t))
			}
		}
	default:
		return nil
	}
	if len(parts) == 0 {
		return nil
	}
	return map[string]any{"role": "system", "content": parts}
}

func textPart(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}

// translateMessage converts one Anthropic message into one or more OpenAI
// messages. tool_result blocks become standalone role:"tool" messages emitted
// before the message's own content, so they immediately follow the assistant
// tool_calls message as OpenAI requires. A system-role message mid-conversation
// (Claude Code injects these) becomes a user message wrapped in
// <system-reminder> tags to prevent prompt injection via role confusion.
func translateMessage(m *caddyllm.LazyJsonNode) ([]any, error) {
	role, _ := m.Get("role").String()
	content := m.Get("content")

	if role == "system" {
		var text string
		switch content.Type() {
		case caddyllm.TypeString:
			text, _ = content.String()
		case caddyllm.TypeArray:
			var texts []string
			if items, ok := content.List(); ok {
				for _, b := range items {
					if b.Type() == caddyllm.TypeObject && blockType(b) == "text" {
						t, _ := b.Get("text").String()
						if strings.TrimSpace(t) != "" {
							texts = append(texts, t)
						}
					}
				}
			}
			text = strings.Join(texts, "\n\n")
		}
		return []any{map[string]any{
			"role": "user",
			"content": []any{textPart(
				"<system-reminder>\n" + text + "\n</system-reminder>"),
			},
		}}, nil
	}

	var out []any

	if content.Type() == caddyllm.TypeString {
		s, _ := content.String()
		return []any{map[string]any{"role": role, "content": s}}, nil
	}
	if content.Type() != caddyllm.TypeArray {
		return nil, nil // null/absent content: message carries nothing
	}
	blocks, _ := content.List()

	// First pass: tool_result blocks -> role:"tool" messages.
	for _, b := range blocks {
		if b.Type() != caddyllm.TypeObject || blockType(b) != "tool_result" {
			continue
		}
		c, err := toolResultContent(b.Get("content"))
		if err != nil {
			id, _ := b.Get("tool_use_id").String()
			return nil, fmt.Errorf("tool_result %s: %w", id, err)
		}
		id, _ := b.Get("tool_use_id").String()
		out = append(out, map[string]any{
			"role": "tool", "tool_call_id": id, "content": c,
		})
	}

	// Second pass: the message's own content.
	own := map[string]any{"role": role}
	var (
		parts     []any
		texts     []string
		thinkings []string
		toolCalls []any
	)
	for _, b := range blocks {
		switch bt := blockType(b); bt {
		case "text":
			t, _ := b.Get("text").String()
			if strings.TrimSpace(t) == "" {
				continue
			}
			if role == "assistant" {
				texts = append(texts, t)
			} else {
				parts = append(parts, textPart(t))
			}
		case "thinking":
			// Only round-trip signed thinking; redacted thinking never crosses.
			sig, _ := b.Get("signature").String()
			if sig == "" {
				continue
			}
			th, _ := b.Get("thinking").String()
			thinkings = append(thinkings, th)
		case "redacted_thinking":
			// dropped
		case "tool_use":
			id, _ := b.Get("id").String()
			name, _ := b.Get("name").String()
			// arguments is a JSON *string* on the OpenAI wire; the input
			// object's bytes pass through verbatim.
			args := "{}"
			if in := b.Get("input"); in.Type() == caddyllm.TypeObject {
				if raw, err := in.Marshal(); err == nil {
					args = string(raw)
				}
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
		case "image", "document":
			part, err := contentPartFromBlock(b, bt)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		default:
			// Unknown block types are dropped.
		}
	}

	if len(thinkings) > 0 {
		own["reasoning_content"] = strings.Join(thinkings, "\n\n")
	}
	if len(texts) > 0 {
		parts = append([]any{textPart(strings.Join(texts, "\n\n"))}, parts...)
	}
	if len(toolCalls) > 0 {
		own["tool_calls"] = toolCalls
	}
	switch {
	case len(parts) > 0:
		own["content"] = parts
	case len(toolCalls) > 0 || len(thinkings) > 0:
		own["content"] = ""
	default:
		return out, nil // nothing of this message survives
	}
	out = append(out, own)
	return out, nil
}

// contentPartFromBlock converts an image or document block into an OpenAI
// image_url content part.
func contentPartFromBlock(b *caddyllm.LazyJsonNode, blockType string) (map[string]any, error) {
	src := b.Get("source")
	if src.Type() != caddyllm.TypeObject {
		return nil, fmt.Errorf("%s block missing source", blockType)
	}
	var url string
	switch st, _ := src.Get("type").String(); st {
	case "base64":
		media, _ := src.Get("media_type").String()
		if media == "" {
			media = "application/octet-stream"
		}
		data, _ := src.Get("data").String()
		url = "data:" + media + ";base64," + data
	case "url":
		url, _ = src.Get("url").String()
	default:
		return nil, fmt.Errorf("unsupported source type %q", st)
	}
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": url},
	}, nil
}

// toolResultContent converts a tool_result block's content field (string or
// block array) into an OpenAI tool-message content value. Text-only arrays
// are joined into a plain string; arrays containing images stay as parts.
func toolResultContent(n *caddyllm.LazyJsonNode) (any, error) {
	switch n.Type() {
	case caddyllm.TypeString:
		s, _ := n.String()
		return s, nil
	case caddyllm.TypeArray:
		var (
			texts  []string
			parts  []any
			hasImg bool
		)
		items, _ := n.List()
		for _, b := range items {
			switch blockType(b) {
			case "text":
				t, _ := b.Get("text").String()
				if hasImg {
					parts = append(parts, textPart(t))
				} else {
					texts = append(texts, t)
				}
			case "image":
				part, err := contentPartFromBlock(b, "image")
				if err != nil {
					return nil, err
				}
				if !hasImg {
					for _, t := range texts {
						parts = append(parts, textPart(t))
					}
					texts = nil
					hasImg = true
				}
				parts = append(parts, part)
			default:
				// dropped
			}
		}
		if !hasImg {
			return strings.Join(texts, "\n\n"), nil
		}
		return parts, nil
	case caddyllm.TypeNull, caddyllm.TypeNotExist:
		return "", nil
	default:
		return nil, fmt.Errorf("parse content: unexpected type")
	}
}

// translateTool converts one Anthropic tool definition into an OpenAI
// function tool, normalizing its input schema (every object-type node gets a
// "properties" key — some upstreams reject object schemas without it).
func translateTool(t *caddyllm.LazyJsonNode) (map[string]any, error) {
	name, ok := t.Get("name").String()
	if !ok {
		return nil, fmt.Errorf("tool missing name")
	}
	fn := map[string]any{"name": name}
	if d, ok := t.Get("description").String(); ok {
		fn["description"] = d
	}
	fn["parameters"] = normalizeSchema(t.Get("input_schema"))
	return map[string]any{
		"type":     "function",
		"function": fn,
	}, nil
}

// normalizeSchema ensures every object-type node in a JSON schema carries a
// "properties" key (as an empty object when absent). The result is a value
// tree; leaf subtrees that need no normalization keep their original bytes
// as RawMessage.
func normalizeSchema(n *caddyllm.LazyJsonNode) any {
	switch n.Type() {
	case caddyllm.TypeObject:
		members, ok := n.Obj()
		if !ok {
			return rawOrNull(n)
		}
		needsProps := false
		if t, ok := members["type"].String(); ok && t == "object" {
			if p, present := members["properties"]; !present || p.Type() == caddyllm.TypeNull {
				needsProps = true
			}
		}
		out := make(map[string]any, len(members)+1)
		for k, v := range members {
			if k == "properties" && needsProps {
				continue // replaced below
			}
			out[k] = normalizeSchema(v)
		}
		if needsProps {
			out["properties"] = map[string]any{}
		}
		return out
	case caddyllm.TypeArray:
		items, _ := n.List()
		out := make([]any, len(items))
		for i, it := range items {
			out[i] = normalizeSchema(it)
		}
		return out
	default:
		return rawOrNull(n)
	}
}

// rawOrNull passes a scalar/leaf node through as its original bytes (the
// outer marshal inlines RawMessage verbatim).
func rawOrNull(n *caddyllm.LazyJsonNode) json.RawMessage {
	raw, err := n.Marshal()
	if err != nil {
		return json.RawMessage("null")
	}
	return raw
}

// translateToolChoice maps the Anthropic tool_choice to OpenAI's form.
func translateToolChoice(n *caddyllm.LazyJsonNode) (any, error) {
	if n.Type() != caddyllm.TypeObject {
		return nil, fmt.Errorf("tool_choice is not an object")
	}
	switch t, _ := n.Get("type").String(); t {
	case "any":
		return "required", nil
	case "tool":
		name, _ := n.Get("name").String()
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": name},
		}, nil
	default: // "auto" and unknown
		return "auto", nil
	}
}

// blockType returns a block's "type" string ("" when absent).
func blockType(b *caddyllm.LazyJsonNode) string {
	t, _ := b.Get("type").String()
	return t
}
