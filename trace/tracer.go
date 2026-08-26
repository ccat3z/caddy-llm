package trace

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/ccat3z/caddy-llm/llmroute"
)

func init() {
	caddy.RegisterModule(Tracer{})
}

// traceIDVar carries the trace id between the tracer stages of one client
// request via caddyhttp vars — shared through the request context (llm_route
// clones keep it), never leaked onto the wire.
const traceIDVar = "llm_trace_id"

// TraceIDHeader returns the trace id to the client (response side only).
const TraceIDHeader = "X-LLM-Trace-ID"

// Tracer records request/response exchanges to the trace store.
type Tracer struct {
	// Stage labels this tracer's position in the chain, e.g. "claude" or
	// "openai".
	Stage string `json:"stage,omitempty"`

	logger *zap.Logger
	app    *Store
}

// CaddyModule returns the Caddy module information.
func (Tracer) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.trace",
		New: func() caddy.Module { return &Tracer{} },
	}
}

// Provision resolves the trace store app.
func (t *Tracer) Provision(ctx caddy.Context) error {
	t.logger = ctx.Logger()
	appIface, err := ctx.App("llm_tracer")
	if err != nil {
		return err
	}
	t.app = appIface.(*Store)
	return nil
}

// store returns the trace store, tolerating app Start not having run yet
// (returns nil; traces are then dropped with a log).
func (t *Tracer) storage() storage {
	if t.app == nil {
		return nil
	}
	return t.app.Storage()
}

// Interface guard
var _ caddyhttp.MiddlewareHandler = (*Tracer)(nil)

func (t *Tracer) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	start := time.Now()

	// Correlate stages: the first tracer mints the ID; later tracers reuse it.
	// The id travels in a request var (context-scoped, survives llm_route's
	// clones) — not a header, so it never reaches upstreams.
	traceID, _ := caddyhttp.GetVar(r.Context(), traceIDVar).(string)
	if traceID == "" {
		traceID = newTraceID()
		caddyhttp.SetVar(r.Context(), traceIDVar, traceID)
	}
	w.Header().Set(TraceIDHeader, traceID)

	s := t.storage()

	// Record the full inbound request as a replayable HTTP/1.1 message:
	// request-line + headers + blank line, then the raw body.
	reqBytes := 0
	if s != nil {
		var head bytes.Buffer
		fmt.Fprintf(&head, "%s %s HTTP/1.1\r\n", r.Method, r.URL.RequestURI())
		// Host lives in r.Host, not r.Header (Go's server moves it); emit it
		// so the stored message is a valid, replayable HTTP/1.1 request
		// (Host is mandatory per RFC 9112 §3.2).
		if r.Host != "" {
			fmt.Fprintf(&head, "Host: %s\r\n", r.Host)
		}
		for k, vs := range r.Header {
			for _, v := range vs {
				fmt.Fprintf(&head, "%s: %s\r\n", k, v)
			}
		}
		head.WriteString("\r\n")
		if err := s.Save(r.Context(), traceID, t.Stage, true, head.Bytes()); err != nil {
			t.logger.Error("save request head", zap.Error(err), zap.String("id", traceID))
			s = nil // storage broken; skip further writes
		}
	}
	var body []byte
	if r.Body != nil {
		if jb, ok := r.Body.(*llmroute.Body); ok {
			// Marshal freezes the wire bytes from the parsed object without
			// disturbing r.Body's read state, so downstream handlers keep
			// both the type to assert against and a readable body — and the
			// bytes are marshaled exactly once.
			raw, err := jb.Marshal()
			if err != nil {
				return err
			}
			body = raw
		} else {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				return err
			}
			body = raw
			// Restore for downstream consumption.
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			r.Header.Set("Content-Length", strconv.Itoa(len(body)))
			r.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		}
	}
	if s != nil && len(body) > 0 {
		if err := s.Save(r.Context(), traceID, t.Stage, true, body); err != nil {
			t.logger.Error("save request body", zap.Error(err), zap.String("id", traceID))
			s = nil
		}
		reqBytes = len(body)
	}

	rw := &teeResponseWriter{ResponseWriter: w, tracer: t, traceID: traceID, stage: t.Stage, storage: s}
	err := next.ServeHTTP(rw, r)

	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	if s != nil {
		if rerr := s.RecordRequest(r.Context(), traceID, t.Stage, start.UTC(),
			int(time.Since(start).Milliseconds()), status, reqBytes, rw.written); rerr != nil {
			t.logger.Error("record request", zap.Error(rerr), zap.String("id", traceID))
		}
	}
	return err
}

// teeResponseWriter passes response bytes through to the client while
// appending them to the trace store incrementally — every Write goes to disk
// immediately, so a crash mid-stream keeps whatever already arrived.
type teeResponseWriter struct {
	http.ResponseWriter
	status  int
	written int

	tracer    *Tracer
	traceID   string
	stage     string
	storage   storage
	headSaved bool
}

func (w *teeResponseWriter) WriteHeader(status int) {
	w.status = status
	if w.storage != nil && !w.headSaved {
		w.headSaved = true
		var head bytes.Buffer
		fmt.Fprintf(&head, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
		for k, vs := range w.Header() {
			if strings.EqualFold(k, "Content-Length") {
				continue // may be stale through fallthrough stacks
			}
			for _, v := range vs {
				fmt.Fprintf(&head, "%s: %s\r\n", k, v)
			}
		}
		head.WriteString("\r\n")
		if err := w.storage.Save(nil, w.traceID, w.stage, false, head.Bytes()); err != nil {
			w.tracer.logger.Error("save response head", zap.Error(err), zap.String("id", w.traceID))
			w.storage = nil
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

// ObserveStatus forwards status observations (e.g. claude2openai's buffered
// translator reporting an upstream error for llm_route's fallthrough
// decision) through the tee, so wrappers further out still see them.
func (w *teeResponseWriter) ObserveStatus(code int) {
	if w.status == 0 {
		w.status = code
	}
	if obs, ok := w.ResponseWriter.(interface{ ObserveStatus(int) }); ok {
		obs.ObserveStatus(code)
	}
}

func (w *teeResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.storage != nil {
		if err := w.storage.Save(nil, w.traceID, w.stage, false, p); err != nil {
			w.tracer.logger.Error("save response chunk", zap.Error(err), zap.String("id", w.traceID))
			w.storage = nil // stop tracing, keep proxying
		}
	}
	w.written += len(p)
	return w.ResponseWriter.Write(p)
}

func (w *teeResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newTraceID() string {
	var b []byte = make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}
