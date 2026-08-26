package trace

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/ccat3z/caddy-llm/llmroute"
)

func init() {
	caddy.RegisterModule(Tracer{})
}

// TraceIDHeader correlates the tracer stages of one client request.
const TraceIDHeader = "X-LLM-Trace-ID"

// maxCapture caps in-memory capture per request/response body.
const maxCapture = 10 << 20 // 10MB

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
	traceID := r.Header.Get(TraceIDHeader)
	if traceID == "" {
		traceID = newTraceID()
		r.Header.Set(TraceIDHeader, traceID)
		w.Header().Set(TraceIDHeader, traceID)
	}

	// Capture the request body while keeping it readable downstream.
	var reqBuf bytes.Buffer
	reqTrunc := false
	if jb, ok := r.Body.(*llmroute.Body); ok {
		// llm_route's parsed body: snapshot the object without touching
		// r.Body, so downstream handlers keep the type to assert against.
		raw, err := json.Marshal(jb.Obj)
		if err != nil {
			return err
		}
		reqBuf.Write(raw)
	} else if r.Body != nil {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxCapture+1))
		if err != nil {
			return err
		}
		if len(body) > maxCapture {
			reqTrunc = true
			body = body[:maxCapture]
		}
		reqBuf.Write(body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(reqBuf.Len())
		r.Header.Set("Content-Length", strconv.Itoa(reqBuf.Len()))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(reqBuf.Bytes())), nil
		}
	}

	rw := &teeResponseWriter{ResponseWriter: w}
	err := next.ServeHTTP(rw, r)

	entry := &Entry{
		ID:              traceID + "/" + t.Stage,
		Stage:           t.Stage,
		Timestamp:       start.UTC(),
		Method:          r.Method,
		Path:            r.URL.Path,
		RequestHeaders:  sanitizeHeaders(r.Header),
		RequestBody:     reqBuf.Bytes(),
		Status:          rw.status,
		ResponseHeaders: sanitizeHeaders(rw.Header()),
		ResponseBody:    rw.buf.Bytes(),
		DurationMS:      time.Since(start).Milliseconds(),
		Truncated:       reqTrunc || rw.truncated,
	}
	if entry.Status == 0 {
		entry.Status = http.StatusOK
	}
	// Record asynchronously — tracing must not add latency.
	go func() {
		s := t.storage()
		if s == nil {
			t.logger.Error("trace store not started; dropping trace", zap.String("id", entry.ID))
			return
		}
		if err := s.Append(nil, entry); err != nil {
			t.logger.Error("append trace", zap.Error(err), zap.String("id", entry.ID))
		}
	}()
	return err
}

// teeResponseWriter passes writes through to the client while capturing a
// bounded copy.
type teeResponseWriter struct {
	http.ResponseWriter
	status    int
	buf       bytes.Buffer
	truncated bool
}

func (w *teeResponseWriter) WriteHeader(status int) {
	w.status = status
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
		w.status = http.StatusOK
	}
	if w.buf.Len() < maxCapture {
		room := maxCapture - w.buf.Len()
		if len(p) > room {
			w.buf.Write(p[:room])
			w.truncated = true
		} else {
			w.buf.Write(p)
		}
	} else {
		w.truncated = true
	}
	return w.ResponseWriter.Write(p)
}

func (w *teeResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// sanitizeHeaders strips credentials before persisting a trace.
func sanitizeHeaders(h http.Header) http.Header {
	out := h.Clone()
	if out.Get("Authorization") != "" {
		out.Set("Authorization", redactKey(out.Get("Authorization")))
	}
	if out.Get("X-Api-Key") != "" {
		out.Set("X-Api-Key", redactKey(out.Get("X-Api-Key")))
	}
	return out
}

// redactKey keeps only the first 4 and last 4 characters.
func redactKey(v string) string {
	if len(v) <= 12 {
		return "****"
	}
	return v[:4] + "..." + v[len(v)-4:]
}

func newTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}
