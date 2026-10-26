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
// Cache counts read tokens only, matching Anthropic semantics: cache
// writes (cache_creation_input_tokens / cache_write_tokens) stay inside
// input.
type Usage struct {
	Input  int `json:"input_tokens"`
	Cache  int `json:"cache_tokens"`
	Output int `json:"output_tokens"`
}

// maxSSEEventSize bounds one buffered SSE event. The event carrying usage can
// be large — an OpenAI Responses terminal event embeds the whole response
// object (tool definitions, instructions, encrypted reasoning) and routinely
// exceeds 64KB — so usage cannot be recovered from a fixed tail window. The
// scanner keeps only the current event instead.
const maxSSEEventSize = 8 << 20

// maxJSONBodySize bounds a buffered non-streaming JSON body.
const maxJSONBodySize = 8 << 20

// usageScanner incrementally extracts the reported usage from a response
// body. SSE bodies are scanned line by line as they arrive, so the buffer
// never has to hold the whole stream; a buffered JSON body is parsed once it
// is complete.
type usageScanner struct {
	mode     scanMode
	json     []byte // buffered JSON body
	line     []byte // current (possibly partial) SSE line
	overflow bool   // current line exceeded maxSSEEventSize
	state    usageState
}

type scanMode uint8

const (
	scanUnknown scanMode = iota
	scanJSON
	scanSSE
	scanIgnored
)

func (s *usageScanner) Write(p []byte) {
	if s.mode == scanUnknown {
		for _, b := range p {
			switch b {
			case ' ', '\t', '\r', '\n':
				continue
			case '{', '[':
				s.mode = scanJSON
			default:
				s.mode = scanSSE
			}
			break
		}
		if s.mode == scanUnknown {
			return // only whitespace so far
		}
	}
	switch s.mode {
	case scanJSON:
		if len(s.json)+len(p) > maxJSONBodySize {
			s.mode = scanIgnored
			s.json = nil
			return
		}
		s.json = append(s.json, p...)
	case scanSSE:
		s.writeSSE(p)
	}
}

func (s *usageScanner) writeSSE(p []byte) {
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendLine(p)
			return
		}
		s.appendLine(p[:i])
		if !s.overflow {
			line := bytes.TrimSuffix(s.line, []byte("\r"))
			if payload, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
				s.state.consumeEvent(payload)
			}
		}
		s.line = s.line[:0]
		s.overflow = false
		p = p[i+1:]
	}
}

func (s *usageScanner) appendLine(b []byte) {
	if s.overflow {
		return
	}
	if len(s.line)+len(b) > maxSSEEventSize {
		s.overflow = true
		s.line = nil
		return
	}
	s.line = append(s.line, b...)
}

// Result returns the usage seen so far, or nil.
func (s *usageScanner) Result() *Usage {
	switch s.mode {
	case scanJSON:
		trimmed := bytes.TrimSpace(s.json)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil
		}
		return bodyUsage(trimmed)
	case scanSSE:
		return s.state.result()
	}
	return nil
}

// extractUsage extracts usage from a complete body. The tracer uses a
// usageScanner directly so it can parse as bytes arrive.
func extractUsage(body []byte) *Usage {
	var s usageScanner
	s.Write(body)
	return s.Result()
}

// usageState accumulates the usage fields reported across SSE events.
type usageState struct {
	in, cache, out int
	seen           bool
}

func (s *usageState) result() *Usage {
	if !s.seen {
		return nil
	}
	return &Usage{Input: s.in, Cache: s.cache, Output: s.out}
}

// consumeEvent folds one SSE data payload into the running usage. Formats:
// Claude message_start (input/cache) + message_delta (final output), an
// OpenAI Responses event with a response.usage object, or an OpenAI
// chat-completions chunk with a usage field.
func (s *usageState) consumeEvent(payload []byte) {
	if bytes.Equal(payload, []byte("[DONE]")) || !json.Valid(payload) {
		return
	}
	if u := openAIUsage(jsonBodyUsage(payload)); u != nil {
		s.set(u)
		return
	}
	var ev struct {
		Type     string `json:"type"`
		Response *struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"response"`
		Message struct {
			Usage struct {
				InputTokens          int `json:"input_tokens"`
				CacheReadInputTokens int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	// OpenAI Responses events (response.created/in_progress/completed/…)
	// nest usage under "response".
	if ev.Response != nil {
		if u := responsesUsage(ev.Response.Usage); u != nil {
			s.set(u)
			return
		}
	}
	switch ev.Type {
	case "message_start":
		s.in = ev.Message.Usage.InputTokens
		s.cache = ev.Message.Usage.CacheReadInputTokens
		s.seen = true
	case "message_delta":
		// Upstreams report the full triple here too; take it when the
		// window lost message_start — nonzero fields win.
		if ev.Usage.InputTokens != 0 || s.in == 0 {
			s.in = ev.Usage.InputTokens
		}
		if ev.Usage.CacheReadInputTokens != 0 || s.cache == 0 {
			s.cache = ev.Usage.CacheReadInputTokens
		}
		s.out = ev.Usage.OutputTokens
		s.seen = true
	}
}

func (s *usageState) set(u *Usage) {
	s.in, s.cache, s.out = u.Input, u.Cache, u.Output
	s.seen = true
}

// bodyUsage extracts usage from a complete JSON response body.
func bodyUsage(doc []byte) *Usage {
	var probe struct {
		Object string          `json:"object"`
		Usage  json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(doc, &probe); err != nil || len(probe.Usage) == 0 {
		return nil
	}
	if probe.Object == "response" {
		return responsesUsage(probe.Usage)
	}
	if u := openAIUsage(probe.Usage); u != nil {
		return u
	}
	return claudeUsage(probe.Usage)
}

// jsonBodyUsage digs the "usage" object out of a JSON document.
func jsonBodyUsage(doc []byte) json.RawMessage {
	var probe struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(doc, &probe); err != nil {
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

// responsesUsage parses an OpenAI Responses usage object. Like chat
// completions, input_tokens includes cached tokens (input_tokens_details.
// cached_tokens); cache writes stay inside input, matching Anthropic
// semantics.
func responsesUsage(raw json.RawMessage) *Usage {
	if len(raw) == 0 {
		return nil
	}
	var u struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	if err := json.Unmarshal(raw, &u); err != nil || (u.InputTokens == 0 && u.OutputTokens == 0) {
		return nil
	}
	return &Usage{Input: u.InputTokens, Cache: u.InputTokensDetails.CachedTokens, Output: u.OutputTokens}
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

// normalize applies the tracer's cache_in_input setting: when the upstream's
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
