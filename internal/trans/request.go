// Package trans translates Anthropic Messages API request bodies into
// OpenAI chat-completions request bodies, operating directly on lazily-parsed
// nodes (internal.LazyJsonNode): only the parts of the request that the
// translation actually reads get parsed; untouched subtrees keep their
// original bytes and are inlined verbatim in the output.
package trans

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/ccat3z/caddy-llm/internal"
)

// TranslateRequest converts an Anthropic Messages API request body into an
// OpenAI chat-completions request body. Both input and output are nodes; the
// output is a segmented object whose untouched members carry the input's
// original bytes.
func TranslateRequest(in *internal.LazyJsonNode) (*internal.LazyJsonNode, error) {
	members, ok := in.Obj()
	if !ok {
		return nil, fmt.Errorf("request body is not a JSON object")
	}
	out := map[string]json.RawMessage{}

	// Scalars pass through; integral numbers keep their integer form.
	if v, ok := members["model"]; ok {
		if s, ok := v.String(); ok {
			out["model"] = json.RawMessage(strconv.Quote(s))
		}
	}
	if v, ok := members["max_tokens"]; ok {
		if n, ok := v.Int(); ok {
			out["max_tokens"] = json.RawMessage(strconv.Itoa(n))
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
	out["stream"] = json.RawMessage(strconv.FormatBool(stream))

	// temperature and top_p are mutually exclusive; temperature wins. A JSON
	// null temperature counts as absent.
	if v, ok := members["temperature"]; ok && v.Type() != internal.TypeNull && v.Type() != internal.TypeNotExist {
		if f, ok := v.Float64(); ok {
			out["temperature"] = json.RawMessage(formatFloat(f))
		}
	} else if v, ok := members["top_p"]; ok && v.Type() != internal.TypeNull && v.Type() != internal.TypeNotExist {
		if f, ok := v.Float64(); ok {
			out["top_p"] = json.RawMessage(formatFloat(f))
		}
	}

	// Stop sequences: one value becomes a bare string, more become an array.
	if v, ok := members["stop_sequences"]; ok {
		if items, ok := v.List(); ok && len(items) > 0 {
			if len(items) == 1 {
				if s, ok := items[0].String(); ok {
					out["stop"] = json.RawMessage(strconv.Quote(s))
				}
			} else {
				parts := make([]string, 0, len(items))
				for _, it := range items {
					s, ok := it.String()
					if !ok {
						return nil, fmt.Errorf("stop_sequences: element is not a string")
					}
					parts = append(parts, strconv.Quote(s))
				}
				out["stop"] = json.RawMessage("[" + strings.Join(parts, ",") + "]")
			}
		}
	}

	if eff, ok := reasoningEffort(members); ok {
		out["reasoning_effort"] = json.RawMessage(strconv.Quote(eff))
	}

	var msgs []json.RawMessage
	if sys := systemMessage(members["system"]); sys != nil {
		msgs = append(msgs, sys)
	}
	if v, ok := members["messages"]; ok {
		items, ok := v.List()
		if !ok && v.Type() == internal.TypeObject {
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
		out["messages"] = json.RawMessage("[" + strings.Join(stringify(msgs), ",") + "]")
	}

	if v, ok := members["tools"]; ok {
		if items, ok := v.List(); ok && len(items) > 0 {
			tools := make([]string, 0, len(items))
			for i, t := range items {
				tool, err := translateTool(t)
				if err != nil {
					return nil, fmt.Errorf("tools[%d]: %w", i, err)
				}
				tools = append(tools, tool)
			}
			out["tools"] = json.RawMessage("[" + strings.Join(tools, ",") + "]")
		}
	}

	if v, ok := members["tool_choice"]; ok {
		tc, err := translateToolChoice(v)
		if err != nil {
			return nil, err
		}
		out["tool_choice"] = json.RawMessage(tc)
	}

	if stream {
		out["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	}

	return &internal.LazyJsonNode{Val: out}, nil
}

func stringify(msgs []json.RawMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = string(m)
	}
	return out
}

// formatFloat keeps integral values integral (2e+06 → 2000000).
func formatFloat(f float64) string {
	if f == float64(int64(f)) && f < 1e15 && f > -1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// reasoningEffort maps the Anthropic thinking config (and output_config for
// adaptive-thinking requests) to an OpenAI reasoning_effort value.
func reasoningEffort(members map[string]*internal.LazyJsonNode) (string, bool) {
	th := members["thinking"]
	if th == nil || th.Type() == internal.TypeNotExist || th.Type() == internal.TypeNull {
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
func systemMessage(n *internal.LazyJsonNode) json.RawMessage {
	if n == nil {
		return nil
	}
	var parts []string
	switch n.Type() {
	case internal.TypeString:
		s, _ := n.String()
		if strings.TrimSpace(s) == "" {
			return nil
		}
		parts = append(parts, partText(s))
	case internal.TypeArray:
		if items, ok := n.List(); ok {
			for _, b := range items {
				if blockType(b) != "text" {
					continue
				}
				t, _ := b.Get("text").String()
				if strings.TrimSpace(t) == "" {
					continue
				}
				parts = append(parts, partText(t))
			}
		}
	default:
		return nil
	}
	if len(parts) == 0 {
		return nil
	}
	return json.RawMessage(`{"role":"system","content":[` + strings.Join(parts, ",") + `]}`)
}

func partText(s string) string {
	q, _ := json.Marshal(s)
	return `{"type":"text","text":` + string(q) + `}`
}

// translateMessage converts one Anthropic message into one or more OpenAI
// messages. tool_result blocks become standalone role:"tool" messages emitted
// before the message's own content, so they immediately follow the assistant
// tool_calls message as OpenAI requires. A system-role message mid-conversation
// (Claude Code injects these) becomes a user message wrapped in
// <system-reminder> tags to prevent prompt injection via role confusion.
func translateMessage(m *internal.LazyJsonNode) ([]json.RawMessage, error) {
	role, _ := m.Get("role").String()
	content := m.Get("content")

	if role == "system" {
		var text string
		switch content.Type() {
		case internal.TypeString:
			text, _ = content.String()
		case internal.TypeArray:
			var texts []string
			if items, ok := content.List(); ok {
				for _, b := range items {
					if b.Type() == internal.TypeObject && blockType(b) == "text" {
						t, _ := b.Get("text").String()
						if strings.TrimSpace(t) != "" {
							texts = append(texts, t)
						}
					}
				}
			}
			text = strings.Join(texts, "\n\n")
		}
		wrapped, _ := json.Marshal([]map[string]any{{
			"type": "text", "text": "<system-reminder>\n" + text + "\n</system-reminder>",
		}})
		return []json.RawMessage{json.RawMessage(`{"role":"user","content":` + string(wrapped) + `}`)}, nil
	}

	var out []json.RawMessage
	blocks, isBlocks := content.List()
	wasString := content.Type() == internal.TypeString

	if wasString {
		s, _ := content.String()
		q, _ := json.Marshal(s)
		return []json.RawMessage{json.RawMessage(`{"role":` + quote(role) + `,"content":` + string(q) + `}`)}, nil
	}
	if content.Type() != internal.TypeArray {
		return nil, nil // null/absent content: message carries nothing
	}
	_ = isBlocks

	// First pass: tool_result blocks -> role:"tool" messages.
	for _, b := range blocks {
		if b.Type() != internal.TypeObject || blockType(b) != "tool_result" {
			continue
		}
		c, err := toolResultContent(b.Get("content"))
		if err != nil {
			id, _ := b.Get("tool_use_id").String()
			return nil, fmt.Errorf("tool_result %s: %w", id, err)
		}
		id, _ := b.Get("tool_use_id").String()
		out = append(out, json.RawMessage(`{"role":"tool","tool_call_id":`+quote(id)+`,"content":`+string(c)+`}`))
	}

	// Second pass: the message's own content.
	var (
		parts     []string
		hasParts  bool
		texts     []string
		hasText   bool
		thinkings []string
		toolCalls []string
	)
	for _, b := range blocks {
		bt := blockType(b)
		switch bt {
		case "text":
			t, _ := b.Get("text").String()
			if strings.TrimSpace(t) == "" {
				continue
			}
			if role == "assistant" {
				texts = append(texts, t)
				hasText = true
			} else {
				q, _ := json.Marshal(t)
				parts = append(parts, `{"type":"text","text":`+string(q)+`}`)
				hasParts = true
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
			args := "{}"
			if in := b.Get("input"); in.Type() == internal.TypeObject {
				if raw, err := in.Marshal(); err == nil {
					args = string(raw)
				}
			}
			q, _ := json.Marshal(args)
			toolCalls = append(toolCalls, `{"id":`+quote(id)+`,"type":"function","function":{"name":`+quote(name)+`,"arguments":`+string(q)+`}}`)
		case "image", "document":
			part, err := contentPartFromBlock(b, bt)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
			hasParts = true
		default:
			// Unknown block types are dropped.
		}
	}

	own := `{"role":` + quote(role)
	if len(thinkings) > 0 {
		r, _ := json.Marshal(strings.Join(thinkings, "\n\n"))
		own += `,"reasoning_content":` + string(r)
	}
	if hasText {
		q, _ := json.Marshal(strings.Join(texts, "\n\n"))
		parts = append([]string{`{"type":"text","text":` + string(q) + `}`}, parts...)
		hasParts = true
	}
	if len(toolCalls) > 0 {
		own += `,"tool_calls":[` + strings.Join(toolCalls, ",") + `]`
	}
	if hasParts {
		own += `,"content":[` + strings.Join(parts, ",") + `]`
	} else if len(toolCalls) > 0 || len(thinkings) > 0 {
		own += `,"content":""`
	} else {
		return out, nil // nothing of this message survives
	}
	own += `}`
	out = append(out, json.RawMessage(own))
	return out, nil
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// contentPartFromBlock converts an image or document block into an OpenAI
// image_url content part.
func contentPartFromBlock(b *internal.LazyJsonNode, blockType string) (string, error) {
	src := b.Get("source")
	if src.Type() != internal.TypeObject {
		return "", fmt.Errorf("%s block missing source", blockType)
	}
	switch st, _ := src.Get("type").String(); st {
	case "base64":
		media, _ := src.Get("media_type").String()
		if media == "" {
			media = "application/octet-stream"
		}
		data, _ := src.Get("data").String()
		u, _ := json.Marshal("data:" + media + ";base64," + data)
		return `{"type":"image_url","image_url":{"url":` + string(u) + `}}`, nil
	case "url":
		u, _ := src.Get("url").String()
		qu, _ := json.Marshal(u)
		return `{"type":"image_url","image_url":{"url":` + string(qu) + `}}`, nil
	default:
		return "", fmt.Errorf("unsupported source type %q", st)
	}
}

// toolResultContent converts a tool_result block's content field (string or
// block array) into an OpenAI tool-message content value. Text-only arrays
// are joined into a plain string; arrays containing images stay as parts.
func toolResultContent(n *internal.LazyJsonNode) (json.RawMessage, error) {
	switch n.Type() {
	case internal.TypeString:
		s, _ := n.String()
		return json.RawMessage(quote(s)), nil
	case internal.TypeArray:
		var (
			texts  []string
			parts  []string
			hasImg bool
		)
		items, _ := n.List()
		for _, b := range items {
			switch blockType(b) {
			case "text":
				t, _ := b.Get("text").String()
				if hasImg {
					q, _ := json.Marshal(t)
					parts = append(parts, `{"type":"text","text":`+string(q)+`}`)
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
						q, _ := json.Marshal(t)
						parts = append(parts, `{"type":"text","text":`+string(q)+`}`)
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
			return json.RawMessage(quote(strings.Join(texts, "\n\n"))), nil
		}
		return json.RawMessage("[" + strings.Join(parts, ",") + "]"), nil
	case internal.TypeNull, internal.TypeNotExist:
		return json.RawMessage(`""`), nil
	default:
		return nil, fmt.Errorf("parse content: unexpected type")
	}
}

// translateTool converts one Anthropic tool definition into an OpenAI
// function tool, normalizing its input schema (every object-type node gets a
// "properties" key — some upstreams reject object schemas without it).
func translateTool(t *internal.LazyJsonNode) (string, error) {
	name, ok := t.Get("name").String()
	if !ok {
		return "", fmt.Errorf("tool missing name")
	}
	qname := quote(name)
	qdesc := ""
	if d, ok := t.Get("description").String(); ok {
		qdesc = `,"description":` + quote(d)
	}
	schema := normalizeSchema(t.Get("input_schema"))
	return `{"type":"function","function":{"name":` + qname + qdesc + `,"parameters":` + string(schema) + `}}`, nil
}

// normalizeSchema recursively ensures every object-type node in a JSON schema
// carries a "properties" key (as an empty object when absent). Untouched
// subtrees keep their original bytes; the result re-encodes only where a
// "properties" key was actually added.
func normalizeSchema(n *internal.LazyJsonNode) json.RawMessage {
	switch n.Type() {
	case internal.TypeObject:
		members, ok := n.Obj()
		if !ok {
			raw, _ := n.Marshal()
			return raw
		}
		needsProps := false
		if t, ok := members["type"].String(); ok && t == "object" {
			if p, present := members["properties"]; !present || p.Type() == internal.TypeNull {
				needsProps = true
			}
		}
		var parts []string
		for k, v := range members {
			if needsProps && k == "properties" {
				continue // replaced below
			}
			if k == "properties" {
				parts = append(parts, quote(k)+":"+string(normalizeSchema(v)))
				continue
			}
			child := normalizeSchema(v)
			parts = append(parts, quote(k)+":"+string(child))
		}
		if needsProps {
			parts = append(parts, `"properties":{}`)
		}
		return json.RawMessage("{" + strings.Join(parts, ",") + "}")
	case internal.TypeArray:
		items, _ := n.List()
		var parts []string
		for _, it := range items {
			parts = append(parts, string(normalizeSchema(it)))
		}
		return json.RawMessage("[" + strings.Join(parts, ",") + "]")
	default:
		raw, _ := n.Marshal()
		return raw
	}
}

// translateToolChoice maps the Anthropic tool_choice to OpenAI's form.
func translateToolChoice(n *internal.LazyJsonNode) (string, error) {
	if n.Type() != internal.TypeObject {
		return "", fmt.Errorf("tool_choice is not an object")
	}
	switch t, _ := n.Get("type").String(); t {
	case "any":
		return `"required"`, nil
	case "tool":
		name, _ := n.Get("name").String()
		return `{"type":"function","function":{"name":` + quote(name) + `}}`, nil
	default: // "auto" and unknown
		return `"auto"`, nil
	}
}

// blockType returns a block's "type" string ("" when absent).
func blockType(b *internal.LazyJsonNode) string {
	t, _ := b.Get("type").String()
	return t
}
