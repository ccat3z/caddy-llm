// Package claudetoopenai provides the Caddy HTTP handler that translates
// Anthropic Messages API requests (/v1/messages) into OpenAI chat-completions
// requests, and translates the upstream response back. Actual forwarding is
// delegated to a reverse_proxy later in the chain.
package claudetoopenai

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/ccat3z/caddy-llm/llmroute"
	"github.com/ccat3z/caddy-llm/translate"
)

func init() {
	caddy.RegisterModule(Claude2OpenAI{})
	httpcaddyfile.RegisterHandlerDirective("claude2openai", parseCaddyfile)
	// Let the directive be used outside route blocks (handle, site level).
	httpcaddyfile.RegisterDirectiveOrder("claude2openai", httpcaddyfile.Before, "reverse_proxy")
}

// Claude2OpenAI translates Anthropic /v1/messages requests to OpenAI
// chat-completions and back. It only rewrites the body and strips
// Anthropic-specific headers; routing and upstream path rewriting belong to
// route matchers and the rewrite handler.
type Claude2OpenAI struct {
	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (Claude2OpenAI) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.claude2openai",
		New: func() caddy.Module { return &Claude2OpenAI{} },
	}
}

// Provision sets up the module.
func (c *Claude2OpenAI) Provision(ctx caddy.Context) error {
	c.logger = ctx.Logger()
	return nil
}

// Interface guard
var _ caddyhttp.MiddlewareHandler = (*Claude2OpenAI)(nil)

const maxBodySize = 128 << 20 // 128MB request/response cap

func (c *Claude2OpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// Translation applies to every POST this handler is chained on; which
	// paths reach it is the route matcher's decision. Non-POST passes through.
	if r.Method != http.MethodPost {
		return next.ServeHTTP(w, r)
	}

	var req translate.AnthropicRequest
	stream := false
	if jb, ok := r.Body.(*llmroute.Body); ok {
		// llm_route already parsed the body: translate the object in place
		// (before anyone reads the bytes). The wire form marshals on demand.
		if jb.Readonly() {
			return c.writeClaudeError(w, http.StatusInternalServerError, "api_error", "request body already consumed before translation")
		}
		raw, err := json.Marshal(jb.Obj)
		if err != nil {
			return c.writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "parse request body: "+err.Error())
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return c.writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "parse request body: "+err.Error())
		}
		openaiReq, err := translate.TranslateRequest(&req)
		if err != nil {
			return c.writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "translate request: "+err.Error())
		}
		rawOut, err := json.Marshal(openaiReq)
		if err != nil {
			return c.writeClaudeError(w, http.StatusInternalServerError, "api_error", err.Error())
		}
		var outMap map[string]any
		if err := json.Unmarshal(rawOut, &outMap); err != nil {
			return c.writeClaudeError(w, http.StatusInternalServerError, "api_error", err.Error())
		}
		stream = req.Stream
		jb.Obj = outMap
	} else {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
		if err != nil {
			return c.writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "read request body: "+err.Error())
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return c.writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "parse request body: "+err.Error())
		}
		openaiReq, err := translate.TranslateRequest(&req)
		if err != nil {
			return c.writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "translate request: "+err.Error())
		}
		outBody, err := json.Marshal(openaiReq)
		if err != nil {
			return c.writeClaudeError(w, http.StatusInternalServerError, "api_error", err.Error())
		}
		// Replace the request for downstream handlers (reverse_proxy).
		r.Body = io.NopCloser(bytes.NewReader(outBody))
		r.ContentLength = int64(len(outBody))
		r.Header.Set("Content-Length", strconv.Itoa(len(outBody)))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(outBody)), nil
		}
		stream = req.Stream
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Del("Anthropic-Version")
	r.Header.Del("Anthropic-Beta")

	var rw *responseWriter
	if stream {
		rw = newStreamResponseWriter(w, req.Model)
	} else {
		rw = newBufferedResponseWriter(w, req.Model)
	}
	if err := next.ServeHTTP(rw, r); err != nil {
		// Handler errors abort the chain; translate to a Claude error body.
		return c.writeClaudeError(w, http.StatusInternalServerError, "api_error", err.Error())
	}
	rw.finish()
	return nil
}

// writeClaudeError writes an Anthropic-style error response without calling
// the rest of the chain.
func (c *Claude2OpenAI) writeClaudeError(w http.ResponseWriter, status int, errType, msg string) error {
	body := translate.TranslateError(status, []byte(`{"error":{"type":"`+errType+`","message":`+jsonString(msg)+`}}`))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// responseWriter wraps the client ResponseWriter and translates the upstream
// OpenAI response into Claude format.
type responseWriter struct {
	w       http.ResponseWriter
	model   string
	stream  bool
	status  int
	written bool

	// buffered (non-stream) mode
	buf     bytes.Buffer
	maxBuff bool

	// streaming mode
	feeder *translate.StreamFeeder
	gz     *gzip.Reader // non-nil when the upstream stream is gzip-encoded
}

func newBufferedResponseWriter(w http.ResponseWriter, model string) *responseWriter {
	return &responseWriter{w: w, model: model}
}

func newStreamResponseWriter(w http.ResponseWriter, model string) *responseWriter {
	return &responseWriter{
		w:      w,
		model:  model,
		stream: true,
		feeder: translate.NewStreamFeeder(model),
	}
}

func (rw *responseWriter) Header() http.Header { return rw.w.Header() }

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	if status != http.StatusOK || !rw.stream {
		// Errors and non-streaming responses are buffered for translation;
		// the real write happens in finish(). The status still must reach
		// the wrapped writer for its decisions (llm_route's peekWriter
		// observes it for fallthrough) — recorded without any real write.
		if obs, ok := rw.w.(interface{ ObserveStatus(int) }); ok {
			obs.ObserveStatus(status)
		}
		rw.buf.Reset()
		return
	}
	// Streaming 200: pass headers through with SSE content type.
	h := rw.w.Header()
	// We translate the body; deliver it uncompressed and say so. Length is
	// unknown up front (and a fallthrough attempt may have left a stale
	// Content-Length from an earlier upstream error in the shared header
	// map), so drop both length headers and stream.
	h.Del("Content-Encoding")
	h.Del("Content-Length")
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	rw.w.WriteHeader(status)
	rw.written = true
}

func (rw *responseWriter) Write(p []byte) (int, error) {


	if rw.stream && rw.status == http.StatusOK {
		return rw.writeStream(p)
	}
	if rw.buf.Len()+len(p) > maxBodySize && !rw.maxBuff {
		rw.maxBuff = true
	}
	if !rw.maxBuff {
		rw.buf.Write(p)
	}
	return len(p), nil
}

// writeStream converts upstream SSE incrementally, flushing per event.
// Gzip-encoded upstream streams are decompressed on the fly.
func (rw *responseWriter) writeStream(p []byte) (int, error) {
	if strings.EqualFold(rw.w.Header().Get("Content-Encoding"), "gzip") {
		if rw.gz == nil {
			var err error
			rw.gz, err = gzip.NewReader(bytes.NewReader(p))
			if err != nil {
				return len(p), nil // not usable gzip; drop
			}
			defer func() { /* reader kept across writes via MultiStream below */ }()
			// decompress this chunk (gzip.Reader reads to EOF of member)
			return rw.feedGzip()
		}
		if err := rw.gz.Reset(bytes.NewReader(p)); err != nil {
			return len(p), nil
		}
		return rw.feedGzip()
	}
	return rw.feed(p)
}

// feedGzip drains the current gzip member into the converter.
func (rw *responseWriter) feedGzip() (int, error) {
	for {
		buf := make([]byte, 4096)
		n, err := rw.gz.Read(buf)
		if n > 0 {
			if _, ferr := rw.feed(buf[:n]); ferr != nil {
				return 0, ferr
			}
		}
		if err == io.EOF {
			return 0, nil
		}
		if err != nil {
			return 0, nil // corrupt stream; stop feeding
		}
	}
}

func (rw *responseWriter) feed(p []byte) (int, error) {
	events, err := rw.feeder.Write(p)
	if err != nil {
		return 0, err
	}
	if len(events) > 0 {
		if _, err := rw.w.Write(translate.EncodeAll(events)); err != nil {
			return 0, err
		}
	}
	if f, ok := rw.w.(http.Flusher); ok {
		f.Flush()
	}
	return len(p), nil
}

// finish translates and writes the buffered response (non-streaming), or
// closes the stream (streaming).
func (rw *responseWriter) finish() {
	if rw.stream {
		if rw.status == http.StatusOK {
			events, err := rw.feeder.Close()
			if err == nil {
				_, _ = rw.w.Write(translate.EncodeAll(events))
			}
			if f, ok := rw.w.(http.Flusher); ok {
				f.Flush()
			}
		}
		return
	}
	if rw.written {
		return
	}
	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	body := rw.buf.Bytes()
	if strings.EqualFold(rw.w.Header().Get("Content-Encoding"), "gzip") {
		if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			if plain, err := io.ReadAll(zr); err == nil {
				body = plain
			}
		}
	}
	var out []byte
	if status >= 200 && status < 300 && len(body) > 0 {
		var resp translate.OpenAIResponse
		if err := json.Unmarshal(body, &resp); err == nil {
			out, _ = json.Marshal(translate.TranslateResponse(&resp, rw.model))
		} else {
			status = http.StatusBadGateway
			out = translate.TranslateError(status, body)
		}
	} else {
		out = translate.TranslateError(status, body)
	}
	h := rw.w.Header()
	h.Del("Content-Encoding") // we translated the (decompressed) body
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(out)))
	rw.w.WriteHeader(status)
	_, _ = rw.w.Write(out)
	rw.written = true
}

// Flush implements http.Flusher so reverse_proxy streaming works.
func (rw *responseWriter) Flush() {
	if f, ok := rw.w.(http.Flusher); ok {
		f.Flush()
	}
}

var _ http.Flusher = (*responseWriter)(nil)

// ---------- Caddyfile ----------

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var c Claude2OpenAI
	for h.Next() {
		// No arguments or blocks: a positional "/" token would be claimed by
		// the Caddyfile adapter as a path matcher, and path rewriting belongs
		// to the rewrite handler anyway.
		if h.NextArg() {
			return nil, h.ArgErr()
		}
	}
	return &c, nil
}
