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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Route{})
	httpcaddyfile.RegisterHandlerDirective("llm_route", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("llm_route", httpcaddyfile.Before, "reverse_proxy")
}

// origModelVar holds the client's original model name once extracted from the
// request body, so subsequent llm_route blocks match against the same value
// (a failed upstream may have received a rewritten model name).
const origModelVar = "llm_orig_model"

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

	// Sub is the subchain executed (on a cloned request) when a rule
	// matches. Any HTTP routes/handlers may be used.
	Sub *caddyhttp.Subroute `json:"sub,omitempty"`

	logger *zap.Logger `json:"-"`
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
	if r.Sub != nil {
		if err := r.Sub.Provision(ctx); err != nil {
			return fmt.Errorf("provisioning llm_route subchain: %v", err)
		}
	}
	return nil
}

// Interface guard
var _ caddyhttp.MiddlewareHandler = (*Route)(nil)

const maxBodySize = 128 << 20 // 128MB

func (r *Route) ServeHTTP(w http.ResponseWriter, req *http.Request, next caddyhttp.Handler) error {
	// Model selection: use the original model (stored by the first llm_route
	// in the chain) so fallbacks match what the client asked for, not what a
	// previous upstream received.
	model := caddyhttp.GetVar(req.Context(), origModelVar)
	modelStr, ok := model.(string)
	if !ok || modelStr == "" {
		if req.Method != http.MethodPost {
			return next.ServeHTTP(w, req)
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, maxBodySize))
		if err != nil || len(body) == 0 {
			return next.ServeHTTP(w, req)
		}
		modelStr = extractModel(body)
		if modelStr == "" {
			return next.ServeHTTP(w, req)
		}
		caddyhttp.SetVar(req.Context(), origModelVar, modelStr)
		// Restore the untouched body for whoever consumes it next.
		resetBody(req, body)
	}

	rule, rewritten := r.match(modelStr)
	if rule == nil {
		// No rule matched: fall through, request untouched.
		return next.ServeHTTP(w, req)
	}

	// Clone the request (headers/URL deep-copied; body read into memory and
	// re-installed independently on both) so this subchain's rewrites never
	// leak to the next fallback candidate.
	origBody, err := io.ReadAll(io.LimitReader(req.Body, maxBodySize))
	if err != nil {
		return next.ServeHTTP(w, req)
	}
	resetBody(req, origBody) // original stays pristine for later fallbacks

	clone := cloneRequest(req)
	if rewritten != modelStr {
		newBody := rewriteModelField(origBody, rewritten)
		resetBody(clone, newBody)
	} else {
		resetBody(clone, origBody)
	}

	// Run the subchain (compiled with llm_route's own next as its tail, so
	// unmatched inner routes fall through out of the subchain), peeking at
	// the status to decide fallthrough.
	pw := &peekWriter{ResponseWriter: w}
	err = r.Sub.ServeHTTP(pw, clone, next)

	if err != nil || isFallthroughStatus(pw.status()) {
		// Drain whatever the failing attempt still has in flight (keeps the
		// upstream connection reusable) and fall through with the original
		// request. The failure body itself has been captured by any trace
		// handler inside the subchain.
		pw.discard()
		return next.ServeHTTP(w, req)
	}
	pw.flushHeader()
	return nil
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

// extractModel pulls the top-level "model" string out of a JSON body without
// full unmarshalling.
func extractModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// rewriteModelField replaces the value of the top-level "model" field.
func rewriteModelField(body []byte, model string) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = json.RawMessage(jsonString(model))
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func jsonString(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// resetBody installs body as the request's body with length/GetBody rewired.
func resetBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

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
	// The clone shares the context, so vars (orig model, trace IDs) carry in.
	return req
}

// ---------- peek writer ----------

// peekWriter intercepts the subchain's response: it records the status code
// without writing to the real writer until llm_route decides the response is
// acceptable. Body bytes are streamed straight through once the header has
// been accepted.
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

func (pw *peekWriter) Write(p []byte) (int, error) {
	if !pw.wroteHeader {
		pw.WriteHeader(http.StatusOK)
	}
	if !pw.headerSent {
		// fallthrough already decided: drop the body
		pw.discarded += int64(len(p))
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
	pw.headerSent = true
	pw.ResponseWriter.WriteHeader(pw.code)
}

// discard marks the response as dropped (fallthrough); already-buffered
// nothing to drain because writes were dropped at the peek layer — the
// upstream connection itself is closed by reverse_proxy.
func (pw *peekWriter) discard() {
	pw.headerSent = false
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

// ---------- Caddyfile ----------

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var r Route
	for h.Next() {
		if h.NextArg() {
			return nil, h.ArgErr()
		}

		// First pass: slice the block into segments; collect model rules and
		// keep the remaining segments for the subchain.
		var subSegments []caddyfile.Segment
		for nesting := h.Nesting(); h.NextBlock(nesting); {
			seg := h.NextSegment()
			if len(seg) == 0 {
				continue
			}
			if string(seg[0].Text) == "model" {
				d := caddyfile.NewDispenser(seg)
				d.Next() // consume "model"
				args := d.RemainingArgs()
				switch len(args) {
				case 1:
					r.Models = append(r.Models, ModelRule{Pattern: args[0]})
				case 2:
					r.Models = append(r.Models, ModelRule{Pattern: args[0], Replace: args[1]})
				default:
					return nil, h.Errf("model takes 1 arg (exact name) or 2 (pattern replacement)")
				}
				continue
			}
			subSegments = append(subSegments, seg)
		}
		if len(r.Models) == 0 {
			return nil, h.Errf("llm_route requires at least one model rule")
		}
		if len(subSegments) == 0 {
			return nil, h.Errf("llm_route requires a subchain (e.g. reverse_proxy)")
		}

		// Second pass: parse the remaining segments as a subroute (standard
		// directive dispatch: matchers, handler ordering, nesting all work).
		sub, err := parseSegmentsAsSubroute(h, subSegments)
		if err != nil {
			return nil, err
		}
		r.Sub = sub
	}
	return &r, nil
}

// parseSegmentsAsSubroute builds a subroute from raw segments by replaying
// them through the standard Caddyfile machinery.
func parseSegmentsAsSubroute(h httpcaddyfile.Helper, segments []caddyfile.Segment) (*caddyhttp.Subroute, error) {
	// Flatten segments back into a token stream wrapped in a synthetic block,
	// then delegate to ParseSegmentAsSubroute via a fresh Helper.
	tokens := make([]caddyfile.Token, 0, 16)
	for _, seg := range segments {
		tokens = append(tokens, seg...)
	}
	d := caddyfile.NewDispenser(tokens)
	h2 := h.WithDispenser(d)
	h2.Prev() // position before the first token? ParseSegmentAsSubroute does its own Next()
	mh, err := httpcaddyfile.ParseSegmentAsSubroute(h2)
	if err != nil {
		return nil, err
	}
	sub, ok := mh.(*caddyhttp.Subroute)
	if !ok {
		return nil, h.Errf("internal: expected Subroute, got %T", mh)
	}
	return sub, nil
}
