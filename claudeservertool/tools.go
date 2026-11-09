package claudeservertool

import (
	"strings"

	jsonbody "github.com/ccat3z/caddy-llm/jsonbody"
)

type toolSpec struct {
	name       string
	result     string
	errorType  string
	properties map[string]any
	required   []string
}

func spec(name, result, errorType string, properties map[string]any, required ...string) toolSpec {
	return toolSpec{name: name, result: result, errorType: errorType, properties: properties, required: required}
}

// An explicit blacklist of server-executed tool families. Unknown types and
// Anthropic-defined client tools are not classified as server tools.
func serverSpecs(kind string) []toolSpec {
	str := func() any { return map[string]any{"type": "string"} }
	switch {
	case strings.HasPrefix(kind, "web_search_"):
		return []toolSpec{spec("web_search", "web_search_tool_result", "web_search_tool_result_error", map[string]any{"query": str()}, "query")}
	case strings.HasPrefix(kind, "web_fetch_"):
		return []toolSpec{spec("web_fetch", "web_fetch_tool_result", "web_fetch_tool_result_error", map[string]any{"url": str()}, "url")}
	case strings.HasPrefix(kind, "tool_search_tool_regex_"):
		return []toolSpec{spec("tool_search_tool_regex", "tool_search_tool_result", "tool_search_tool_result_error", map[string]any{"pattern": str()}, "pattern")}
	case strings.HasPrefix(kind, "tool_search_tool_bm25_"):
		return []toolSpec{spec("tool_search_tool_bm25", "tool_search_tool_result", "tool_search_tool_result_error", map[string]any{"query": str()}, "query")}
	case strings.HasPrefix(kind, "advisor_"):
		return []toolSpec{spec("advisor", "advisor_tool_result", "advisor_tool_result_error", map[string]any{"message": str()}, "message")}
	case strings.HasPrefix(kind, "code_execution_"):
		if kind == "code_execution_20250522" {
			return []toolSpec{spec("code_execution", "code_execution_tool_result", "code_execution_tool_result_error", map[string]any{"code": str()}, "code")}
		}
		return []toolSpec{
			spec("bash_code_execution", "bash_code_execution_tool_result", "bash_code_execution_tool_result_error", map[string]any{"command": str()}, "command"),
			spec("text_editor_code_execution", "text_editor_code_execution_tool_result", "text_editor_code_execution_tool_result_error", map[string]any{"command": str(), "path": str()}, "command", "path"),
		}
	}
	return nil
}

func prepareRequest(root *jsonbody.LazyJsonNode) (*jsonbody.LazyJsonNode, map[string]toolSpec) {
	registry := make(map[string]toolSpec)
	defs, _ := root.Get("tools").List()
	out := make([]any, 0, len(defs))
	for _, def := range defs {
		kind, _ := def.Get("type").String()
		specs := serverSpecs(kind)
		if len(specs) == 0 {
			out = append(out, def.Val)
			continue
		}
		for _, s := range specs {
			registry[s.name] = s
			out = append(out, map[string]any{
				"name": s.name, "description": "Use this server-side tool when needed. Its execution is handled by the gateway.",
				"input_schema": map[string]any{"type": "object", "properties": s.properties, "required": s.required},
			})
		}
	}
	if len(registry) == 0 {
		return root, registry
	}
	prepared := root.With("tools", out)
	// Normalize each assistant tool batch and its results without placing
	// user text or assistant tail text between calls and their results.
	messages, _ := root.Get("messages").List()
	converted := make([]any, 0, len(messages))
	var pending, tail []any
	flush := func() {
		if len(pending) > 0 {
			converted = append(converted, map[string]any{"role": "user", "content": pending})
			pending = nil
		}
		if len(tail) > 0 {
			converted = append(converted, map[string]any{"role": "assistant", "content": tail})
			tail = nil
		}
	}
	for _, message := range messages {
		role, _ := message.Get("role").String()
		blocks, ok := message.Get("content").List()
		if !ok {
			flush()
			converted = append(converted, message.Val)
			continue
		}
		if role != "assistant" {
			content := make([]any, 0, len(blocks))
			for _, block := range blocks {
				kind, _ := block.Get("type").String()
				if role == "user" && kind == "tool_result" && len(pending) > 0 {
					pending = append(pending, block.Val)
				} else {
					content = append(content, block.Val)
				}
			}
			flush()
			if len(content) > 0 {
				converted = append(converted, message.With("content", content).Val)
			}
			continue
		}
		flush()
		content := make([]any, 0, len(blocks))
		for _, block := range blocks {
			kind, _ := block.Get("type").String()
			name, _ := block.Get("name").String()
			if kind == "server_tool_use" {
				if _, known := registry[name]; known {
					content = append(content, block.With("type", "tool_use").Val)
					continue
				}
			}
			matched := false
			for _, s := range registry {
				if kind == s.result {
					id, _ := block.Get("tool_use_id").String()
					raw, _ := block.Get("content").Marshal()
					pending = append(pending, map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": true, "content": string(raw)})
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			if len(pending) > 0 && kind != "tool_use" {
				tail = append(tail, block.Val)
			} else {
				content = append(content, block.Val)
			}
		}
		if len(content) > 0 {
			converted = append(converted, message.With("content", content).Val)
		}
	}
	flush()
	return prepared.With("messages", converted), registry
}

func (s toolSpec) unavailable(id any) map[string]any {
	errorContent := map[string]any{"type": s.errorType, "error_code": "unavailable"}
	if s.result == "text_editor_code_execution_tool_result" || s.result == "tool_search_tool_result" {
		errorContent["error_message"] = "This server-side tool is unavailable: the gateway does not support its execution."
	}
	return map[string]any{"type": s.result, "tool_use_id": id, "caller": map[string]any{"type": "direct"}, "content": errorContent}
}
