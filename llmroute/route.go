// Package llmroute provides the llm_route handler: model-based upstream
// routing with fallthrough. Each llm_route block declares model match rules
// and its own handler subchain (rewrite / trace / claude2openai /
// reverse_proxy) for one upstream. Requests whose model matches run the
// subchain on a cloned request; if the upstream answers 429/404/5xx (or the
// subchain errors), the response is discarded and the original request falls
// through to the next handler in the chain (typically the next llm_route,
// i.e. the next-lower-priority upstream).
package llmroute

import (
	"encoding/json"
	"fmt"
	caddyllm "github.com/ccat3z/caddy-llm"
	"net/http"
	"net/url"
	"regexp"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Route{})
}

// fallthroughStatuses are the upstream response codes that trigger
// fallthrough to the next llm_route. 5xx is implied.
func isFallthroughStatus(code int) bool {
	return code == http.StatusTooManyRequests ||
		code == http.StatusNotFound ||
		code >= 500
}

// Route is the llm_route handler.
type Route struct {
	// Models are the match rules, in order: a regexp with an optional
	// replacement template ($1 for capture groups), or a bare literal for
	// exact matching. First matching rule wins; its replacement (if any)
	// rewrites the model name sent to this upstream.
	Models []ModelRule `json:"models,omitempty"`

	// SubRaw is the subchain executed (on a cloned request) when a rule
	// matches — a standard subroute module. Loaded via the module machinery
	// so the inline "handler" key is stripped before strict decoding.
	SubRaw json.RawMessage `json:"sub,omitempty" caddy:"namespace=http.handlers inline_key=handler"`

	logger *zap.Logger         `json:"-"`
	sub    *caddyhttp.Subroute `json:"-"`
}

// ModelRule matches the client model name and optionally rewrites it.
type ModelRule struct {
	// Pattern is either a regexp (when Replace is set) or a literal
	// (exact match when Replace is empty).
	Pattern string `json:"pattern,omitempty"`
	// Replace is a regexp replacement template ($1 etc.). Empty means the
	// model name passes through unchanged.
	Replace string `json:"replace,omitempty"`

	re *regexp.Regexp `json:"-"`
}

// CaddyModule returns the Caddy module information.
func (Route) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.llm_route",
		New: func() caddy.Module { return new(Route) },
	}
}

// Provision sets up the module.
func (r *Route) Provision(ctx caddy.Context) error {
	r.logger = ctx.Logger()
	for i := range r.Models {
		m := &r.Models[i]
		if m.Replace != "" {
			re, err := regexp.Compile("^(?:" + m.Pattern + ")$")
			if err != nil {
				return fmt.Errorf("model pattern %q: %v", m.Pattern, err)
			}
			m.re = re
		}
	}
	if r.SubRaw == nil {
		// Without a subchain every matching request would panic on the nil
		// Subroute — fail at load time instead.
		return fmt.Errorf("llm_route requires a sub (subroute) alongside its models")
	}
	mod, err := ctx.LoadModule(r, "SubRaw")
	if err != nil {
		return fmt.Errorf("loading subchain: %v", err)
	}
	sub, ok := mod.(*caddyhttp.Subroute)
	if !ok {
		return fmt.Errorf("subchain must be a subroute, got %T", mod)
	}
	if err := sub.Provision(ctx); err != nil {
		return fmt.Errorf("provisioning llm_route subchain: %v", err)
	}
	r.sub = sub
	return nil
}

// Interface guard
var _ caddyhttp.MiddlewareHandler = (*Route)(nil)

func (r *Route) ServeHTTP(w http.ResponseWriter, req *http.Request, next caddyhttp.Handler) error {
	// The body is read and parsed exactly once per request, here, and
	// re-installed as a *Body: later handlers (llm_route fallback blocks,
	// claude2openai, tracers) can type-assert it and work on the parsed
	// object instead of re-reading bytes. The original request always keeps
	// a readable body — downstream handlers must never see a drained body.
	// A failed upstream may have received a rewritten model name, so blocks
	// always match against the original object's model, never a clone's.
	if req.Method != http.MethodPost {
		return next.ServeHTTP(w, req)
	}
	body, err := caddyllm.FromBody(req)
	if err != nil {
		// Not a JSON object (or empty): pass through untouched —
		// FromBody has already re-installed the raw bytes.
		return next.ServeHTTP(w, req)
	}

	modelStr, _ := body.Get("model").String()
	rule, rewritten := r.match(modelStr)
	if rule == nil {
		// No rule matched: fall through, request untouched.
		return next.ServeHTTP(w, req)
	}

	// The clone's body: bodies are immutable, so a rule without a rewrite
	// shares the original — a fallthrough still sees it pristine, because
	// any handler that "changes" the body installs a new one on the request
	// instead of mutating. A rewrite replaces just the top-level model key;
	// untouched members keep their original bytes (With builds the variant
	// over the node's one-level segmentation).
	clone := cloneRequest(req)
	cloneBody := body
	if rewritten != modelStr {
		variant := body.With("model", rewritten)
		cloneBody = &caddyllm.JsonReqBody{LazyJsonNode: *variant}
	}
	// Always reinstall as a set: the struct copy in cloneRequest would
	// otherwise carry the original request's GetBody (serving the original
	// body) alongside the clone's Body.
	caddyllm.SetRequestBody(clone, cloneBody)

	// Writer stack (response side, outermost first):
	//   peekWriter — llm_route's fallthrough decision on the upstream status
	// The subchain writes into the peek-wrapped real writer (translating
	// handlers like claude2openai sit inside the subchain and wrap it
	// themselves).
	pw := newPeekWriter(w)
	err = r.sub.ServeHTTP(pw, clone, next)

	if err == nil && !isFallthroughStatus(pw.status()) {
		pw.flushHeader()
		return nil
	}

	// The attempt failed. If any of its response already reached the client
	// (status accepted and flushed, or body bytes streamed), falling through
	// would concatenate the fallback's response onto the committed one —
	// return the error instead and let Caddy abort the exchange.
	if pw.headerSent {
		pw.discard()
		if err == nil {
			err = fmt.Errorf("llm_route: upstream failed (%d) after the response was already committed", pw.status())
		}
		return err
	}

	// Nothing reached the client: discard the held-back response, restore the
	// header map, and fall through with the pristine original request.
	pw.discard()
	return next.ServeHTTP(w, req)
}

// match returns the first matching rule and the rewritten model name.
func (r *Route) match(model string) (*ModelRule, string) {
	for i := range r.Models {
		m := &r.Models[i]
		if m.re != nil {
			if m.re.MatchString(model) {
				return m, m.re.ReplaceAllString(model, m.Replace)
			}
			continue
		}
		if m.Pattern == model {
			return m, model
		}
	}
	return nil, ""
}

// ---------- helpers ----------

// cloneRequest makes a semi-deep clone (headers/URL deep, rest shallow), the
// same technique as reverseproxy's cloneRequest.
func cloneRequest(origReq *http.Request) *http.Request {
	req := new(http.Request)
	*req = *origReq
	if origReq.URL != nil {
		newURL := new(url.URL)
		*newURL = *origReq.URL
		newURL.Scheme = ""
		newURL.Host = ""
		req.URL = newURL
	}
	if origReq.Header != nil {
		req.Header = origReq.Header.Clone()
	}
	if origReq.Trailer != nil {
		req.Trailer = origReq.Trailer.Clone()
	}
	// The clone shares the context, so vars (trace IDs) carry in.
	return req
}

// ---------- peek writer ----------

// peekWriter intercepts the subchain's response: it records the status code
// without writing to the real writer until llm_route decides the response is
// acceptable. Body bytes are streamed straight through once the header has
// been accepted.
//
// The wrapped writer's header map is shared across fallthrough attempts
// (reverse_proxy copies upstream headers into it via Add). peekWriter
// therefore snapshots the map before the subchain runs and restores it on
// discard, so a failed attempt's headers never leak into the fallback's
// response (duplicate Content-Length / stale Content-Encoding).
type peekWriter struct {
	http.ResponseWriter
	wroteHeader bool
	// statusCode is the status the subchain wrote (0 if none yet).
	code int
	// headerSent marks that the real writer received the status/headers
	// (only after llm_route accepted the response).
	headerSent bool
	// discarded counts body bytes dropped after a fallthrough decision.
	discarded int64
	// held buffers body written while the header is still held back; it is
	// flushed by flushHeader (streaming bodies never take this path: the
	// status reaches WriteHeader directly and flushes immediately).
	held [][]byte
	// savedHeader is the header map as it was before the subchain ran.
	savedHeader http.Header
}

// newPeekWriter wraps w and snapshots its header map for fallthrough restores.
func newPeekWriter(w http.ResponseWriter) *peekWriter {
	return &peekWriter{
		ResponseWriter: w,
		savedHeader:    w.Header().Clone(),
	}
}

func (pw *peekWriter) WriteHeader(code int) {
	if pw.wroteHeader {
		return
	}
	pw.wroteHeader = true
	pw.code = code
	if isFallthroughStatus(code) {
		return // hold the header back; llm_route will fall through
	}
	pw.flushHeader()
}

// ObserveStatus records a status for fallthrough decisions WITHOUT going
// through the real write path. Buffered translators use this so a later
// Finish()/WriteHeader on the same stack doesn't double-write.
func (pw *peekWriter) ObserveStatus(code int) {
	if !pw.wroteHeader {
		pw.wroteHeader = true
		pw.code = code
	}
}

func (pw *peekWriter) Write(p []byte) (int, error) {
	if !pw.wroteHeader {
		pw.WriteHeader(http.StatusOK)
	}
	if !pw.headerSent {
		if pw.code != 0 && isFallthroughStatus(pw.code) {
			// a fallthrough status was observed: drop the body
			pw.discarded += int64(len(p))
			return len(p), nil
		}
		// Header not yet flushed (e.g. observed via ObserveStatus): hold the
		// body back; flushHeader writes it once the status is accepted.
		pw.held = append(pw.held, p)
		return len(p), nil
	}
	return pw.ResponseWriter.Write(p)
}

// status returns the recorded status (0 if none was written).
func (pw *peekWriter) status() int { return pw.code }

// flushHeader writes the held-back status/headers to the real writer.
func (pw *peekWriter) flushHeader() {
	if pw.headerSent || !pw.wroteHeader {
		return
	}
	if isFallthroughStatus(pw.code) {
		// The subchain finished without llm_route falling through (e.g. it
		// handled the error itself): emit what we held back.
		pw.held = nil
		return
	}
	pw.headerSent = true
	pw.ResponseWriter.WriteHeader(pw.code)
	for _, b := range pw.held {
		pw.ResponseWriter.Write(b)
	}
	pw.held = nil
}

// discard drops the failed attempt's response: the held-back header/body is
// released, and the shared header map is restored to its pre-attempt state
// so the next attempt starts clean (reverse_proxy Adds upstream headers into
// the same map; without the restore they leak into the fallback's response
// as duplicates). It cannot un-send a header/body already flushed to the
// client — callers must not fall through after headerSent.
func (pw *peekWriter) discard() {
	pw.headerSent = false
	pw.held = nil
	h := pw.Header()
	for k := range h {
		delete(h, k)
	}
	for k, vs := range pw.savedHeader {
		h[k] = vs
	}
}

// Flush streams through once accepted; before that it is a no-op so SSE
// chunk boundaries do not leak a rejected header.
func (pw *peekWriter) Flush() {
	if pw.headerSent {
		if f, ok := pw.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}

var _ http.Flusher = (*peekWriter)(nil)
