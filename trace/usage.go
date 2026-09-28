package trace

import (
	"bytes"
	"encoding/json"
)

// Usage is the normalized token accounting of one traced exchange: input,
// cache, and output tokens as three independent counts — input never
// includes cache. Extraction reports what the response claims; normalization
// (Tracer.CacheInInput) subtracts cache from input for upstreams whose
// reported input includes it.
//
// Cache counts read tokens only, matching Anthropic/CPA semantics: cache
// writes (cache_creation_input_tokens / cache_write_tokens) stay inside
// input.
type Usage struct {
	Input  int `json:"input_tokens"`
	Cache  int `json:"cache_tokens"`
	Output int `json:"output_tokens"`
}

// tailSize bounds how much of the response body the tracer keeps for usage
// extraction. Usage is always at the end — the final message_delta (Claude
// SSE), the terminal chunk (OpenAI SSE), or the usage field (JSON bodies) —
// so a tail window suffices even for large streams.
const tailSize = 64 << 10

// tailBuffer keeps the last tailSize bytes of a stream.
type tailBuffer struct {
	buf []byte
}

func (b *tailBuffer) Write(p []byte) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > tailSize {
		b.buf = b.buf[len(b.buf)-tailSize:]
	}
}

// usage extracts the reported usage from a response body. Returns nil when
// the body carries no usage (errors, probes, truncated streams). Detection
// is format-based, independent of CacheInInput: Claude JSON/SSE reports
// input_tokens excluding cache; OpenAI chat-completions (JSON or SSE chunks)
// reports prompt_tokens including cached_tokens.
func extractUsage(body []byte) *Usage {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' && json.Valid(trimmed) {
		// Whole JSON body (buffered non-streaming response).
		if u := openAIUsage(jsonBodyUsage(trimmed)); u != nil {
			return u
		}
		return claudeUsage(jsonBodyUsage(trimmed))
	}
	return extractUsageSSE(trimmed)
}

// extractUsageSSE scans data: payloads and keeps usage from the events that
// carry it: Claude message_start (input/cache) + message_delta (final
// output), or the last OpenAI chunk with a usage field.
func extractUsageSSE(body []byte) *Usage {
	var in, cache, out int
	seen := false
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))
		if bytes.Equal(payload, []byte("[DONE]")) || !json.Valid(payload) {
			continue
		}
		if u := openAIUsage(jsonBodyUsage(payload)); u != nil {
			in, cache, out = u.Input, u.Cache, u.Output
			seen = true
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Usage struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(payload, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			in = ev.Message.Usage.InputTokens
			cache = ev.Message.Usage.CacheReadInputTokens
			seen = true
		case "message_delta":
			// Upstreams report the full triple here too; take it when the
			// window lost message_start (tail capture) — nonzero fields win.
			if ev.Usage.InputTokens != 0 || in == 0 {
				in = ev.Usage.InputTokens
			}
			if ev.Usage.CacheReadInputTokens != 0 || cache == 0 {
				cache = ev.Usage.CacheReadInputTokens
			}
			out = ev.Usage.OutputTokens
			seen = true
		}
	}
	if !seen {
		return nil
	}
	return &Usage{Input: in, Cache: cache, Output: out}
}

// jsonBodyUsage digs the "usage" object out of a JSON document.
func jsonBodyUsage(doc []byte) json.RawMessage {
	var probe struct {
		Object string          `json:"object"`
		Type   string          `json:"type"`
		Usage  json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(doc, &probe); err != nil || len(probe.Usage) == 0 {
		return nil
	}
	return probe.Usage
}

// openAIUsage parses an OpenAI chat-completion usage object (prompt_tokens
// includes cached_tokens). Returns nil if it isn't one.
func openAIUsage(raw json.RawMessage) *Usage {
	if len(raw) == 0 {
		return nil
	}
	var u struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		PromptDetails    struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(raw, &u); err != nil || u.PromptTokens == 0 {
		return nil
	}
	return &Usage{Input: u.PromptTokens, Cache: u.PromptDetails.CachedTokens, Output: u.CompletionTokens}
}

// claudeUsage parses a Claude Messages usage object (input_tokens excludes
// cache; cache reads counted separately, writes stay in input).
func claudeUsage(raw json.RawMessage) *Usage {
	if len(raw) == 0 {
		return nil
	}
	var u struct {
		InputTokens          int `json:"input_tokens"`
		OutputTokens         int `json:"output_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
	}
	if err := json.Unmarshal(raw, &u); err != nil || (u.InputTokens == 0 && u.OutputTokens == 0) {
		return nil
	}
	return &Usage{Input: u.InputTokens, Cache: u.CacheReadInputTokens, Output: u.OutputTokens}
}

// normalize applies the stage's cache_in_input setting: when the upstream's
// reported input already includes cache tokens, subtract them so the stored
// triple is always independent.
func (u *Usage) normalize(cacheInInput bool) *Usage {
	if u == nil {
		return nil
	}
	if cacheInInput && u.Cache > 0 {
		u.Input -= u.Cache
		if u.Input < 0 {
			u.Input = 0
		}
	}
	return u
}
