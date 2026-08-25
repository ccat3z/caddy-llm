package claudetoopenai

import (
	"encoding/json"
	"net/http"

	"github.com/caddyserver/caddy/v2"

	"github.com/ccat3z/caddy-llm/llmroute"
	"github.com/ccat3z/caddy-llm/translate"
)

// Rewriter is the rewrite_body plugin form of the claude2openai translation:
// request bodies are translated from Anthropic to OpenAI format inside
// llm_route's single-parse pipeline, and upstream responses are translated
// back via the existing responseWriter machinery (streaming, buffered,
// gzip-aware).
type Rewriter struct{}

// CaddyModule returns the Caddy module information.
func (Rewriter) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.llm_rewriter.claude2openai",
		New: func() caddy.Module { return new(Rewriter) },
	}
}

func init() {
	caddy.RegisterModule(Rewriter{})
}

// Interface guards
var (
	_ caddy.Provisioner     = (*Rewriter)(nil)
	_ llmroute.BodyRewriter = (*Rewriter)(nil)
)

// Provision sets up the module.
func (rw *Rewriter) Provision(ctx caddy.Context) error { return nil }

// RewriteRequest translates a parsed Anthropic request body into a parsed
// OpenAI chat-completions body, preserving the (already rule-rewritten)
// model name.
func (rw *Rewriter) RewriteRequest(model string, body map[string]any) (map[string]any, error) {
	// Reuse the struct-based translator (validated against the real-log
	// corpus): one marshal to enter it, one parse to leave it. Both stay
	// inside llm_route's single body pipeline.
	raw, err := json.Marshal(body)
	if err != nil {
		return body, err
	}
	var in translate.AnthropicRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return body, err
	}
	in.Model = model
	out, err := translate.TranslateRequest(&in)
	if err != nil {
		return body, err
	}
	rawOut, err := json.Marshal(out)
	if err != nil {
		return body, err
	}
	var outMap map[string]any
	if err := json.Unmarshal(rawOut, &outMap); err != nil {
		return body, err
	}
	return outMap, nil
}

// NewResponseTranslator returns the translation writer for this attempt.
func (rw *Rewriter) NewResponseTranslator(model string, stream bool) llmroute.ResponseTranslator {
	if stream {
		return &translatorWriter{rw: newStreamResponseWriter(nil, model)}
	}
	return &translatorWriter{rw: newBufferedResponseWriter(nil, model)}
}

// translatorWriter adapts the handler's responseWriter to
// llmroute.ResponseTranslator (Reset attaches the real writer before the
// subchain runs).
type translatorWriter struct {
	rw *responseWriter
}

func (t *translatorWriter) Reset(w http.ResponseWriter) { t.rw.w = w }

func (t *translatorWriter) Header() http.Header { return t.rw.Header() }

func (t *translatorWriter) WriteHeader(code int) { t.rw.WriteHeader(code) }

func (t *translatorWriter) Write(p []byte) (int, error) { return t.rw.Write(p) }

func (t *translatorWriter) Flush() { t.rw.Flush() }

func (t *translatorWriter) Finish() { t.rw.finish() }
