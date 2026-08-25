package llmroute

import (
	"net/http"
)

// BodyRewriter rewrites a parsed request body inside llm_route's
// rewrite_body pipeline. Implementations are Caddy modules in the
// http.handlers.llm_rewriter namespace.
type BodyRewriter interface {
	// RewriteRequest transforms the parsed JSON request body. The model
	// argument is the (already rule-rewritten) model name; the body map may
	// be modified in place or replaced by the return value.
	RewriteRequest(model string, body map[string]any) (map[string]any, error)

	// NewResponseTranslator returns a transformer wrapping the real
	// ResponseWriter for the subchain (the subchain writes the upstream-format
	// response into it; llm_route serves the client from it), or nil when the
	// rewriter has no response side. stream reports whether the request asked
	// for SSE.
	//
	// Only the FIRST rewriter returning a non-nil translator takes effect:
	// bodies have one format, so there is one translation.
	NewResponseTranslator(model string, stream bool) ResponseTranslator
}

// ResponseTranslator wraps the client ResponseWriter for the duration of the
// subchain. Finish is called after the subchain returns successfully.
type ResponseTranslator interface {
	// The subchain writes the upstream-format response into this writer.
	http.ResponseWriter
	// Reset attaches the real (already peek-wrapped) writer to translate
	// into, starting a fresh translation. Called once, before the subchain.
	Reset(w http.ResponseWriter)
	// Finish translates and flushes any buffered remainder. Not called when
	// llm_route falls through (the attempt is discarded instead).
	Finish()
}
