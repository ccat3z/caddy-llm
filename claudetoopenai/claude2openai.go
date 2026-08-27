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
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	caddyllm "github.com/ccat3z/caddy-llm"
)

func init() {
	caddy.RegisterModule(Claude2OpenAI{})
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

	// The body always ends up as an llmroute body: one that llm_route
	// installed is reused, anything else is read once and parsed here.
	// Translation installs a NEW body with the translated object — bodies
	// are immutable, so the original stays intact for anyone holding it
	// (and a fallthrough up the chain sees the pristine request).
	jb, err := caddyllm.FromBody(r)
	if err != nil {
		return c.writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "read request body: "+err.Error())
	}
	stream, _ := jb.Get("stream").Bool()
	model, _ := jb.Get("model").String()
	outNode, err := TranslateRequest(&jb.LazyJsonNode)
	if err != nil {
		return c.writeClaudeError(w, http.StatusBadRequest, "invalid_request_error", "translate request: "+err.Error())
	}
	caddyllm.SetRequestBody(r, &caddyllm.JsonReqBody{LazyJsonNode: *outNode})
	r.Header.Set("Content-Type", "application/json")
	r.Header.Del("Anthropic-Version")
	r.Header.Del("Anthropic-Beta")

	var rw *responseWriter
	if stream {
		rw = newStreamResponseWriter(w, model)
	} else {
		rw = newBufferedResponseWriter(w, model)
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
	body := TranslateError(status, []byte(`{"error":{"type":"`+errType+`","message":`+jsonString(msg)+`}}`))
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
	feeder *StreamFeeder
	// gzGzip marks a gzip-encoded upstream stream (captured at WriteHeader,
	// before Content-Encoding is deleted from the header map).
	gzGzip bool
	// gzSrc feeds compressed bytes to gz across Write calls, so a gzip
	// member split over multiple Writes still decodes. gzDone closes when
	// the pump goroutine has drained everything (finish waits on it).
	gzSrc  *chunkBuf
	gzDone chan struct{}
	gz     *gzip.Reader
}

// gzEnabled reports whether the upstream stream is gzip-encoded.
func (rw *responseWriter) gzEnabled() bool { return rw.gzGzip }

// chunkBuf is a blocking single-writer/single-reader byte bridge: the pump
// goroutine's Read blocks until the handler goroutine Writes (or closes).
type chunkBuf struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
	// readOff is how many bytes of buf were already consumed.
	readOff int
}

func newChunkBuf() *chunkBuf {
	c := &chunkBuf{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (c *chunkBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	c.buf.Write(p)
	c.cond.Signal()
	return len(p), nil
}

func (c *chunkBuf) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.cond.Broadcast()
	return nil
}

func (c *chunkBuf) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.buf.Len() == c.readOff {
		if c.closed {
			return 0, io.EOF
		}
		c.cond.Wait()
	}
	n := copy(p, c.buf.Bytes()[c.readOff:])
	c.readOff += n
	return n, nil
}

func newBufferedResponseWriter(w http.ResponseWriter, model string) *responseWriter {
	return &responseWriter{w: w, model: model}
}

func newStreamResponseWriter(w http.ResponseWriter, model string) *responseWriter {
	return &responseWriter{
		w:      w,
		model:  model,
		stream: true,
		feeder: NewStreamFeeder(model),
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
	// Capture the upstream encoding before deleting the header: the body is
	// gzip-compressed and must be decompressed before translation, even
	// though we deliver the translated SSE uncompressed.
	if strings.EqualFold(h.Get("Content-Encoding"), "gzip") {
		rw.gzGzip = true // actual reader attached lazily on first body Write
	}
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
// Gzip-encoded upstream streams are decompressed on the fly: the compressed
// bytes are piped into a single gzip.Reader spanning all Writes (attached
// lazily on the first Write — gzip.NewReader blocks until header bytes are
// available), so a member split across chunks still decodes.
func (rw *responseWriter) writeStream(p []byte) (int, error) {
	if rw.gzEnabled() {
		if rw.gzSrc == nil {
			// First Write: attach the decompressor. The reader is created on
			// the pump goroutine (gzip.NewReader blocks until header bytes
			// arrive — which is exactly the first Write below).
			rw.gzSrc = newChunkBuf()
			rw.gzDone = make(chan struct{})
			go rw.pumpGzip()
		}
		if _, err := rw.gzSrc.Write(p); err != nil {
			return len(p), nil // decompressor stopped; drop the rest
		}
		return len(p), nil
	}
	return rw.feed(p)
}

// pumpGzip owns the gzip.Reader: it is created here (not on the handler
// goroutine) because its constructor reads the stream header, which only
// becomes available once the first Write lands. Decompressed bytes are fed
// into the SSE converter — the pump is the ONLY caller of feed on the gzip
// path (the handler goroutine never touches the feeder directly while it
// runs). It exits when the bridge closes (finish) or the stream errors.
func (rw *responseWriter) pumpGzip() {
	defer close(rw.gzDone)
	gzr, err := gzip.NewReader(rw.gzSrc)
	if err != nil {
		return // not usable gzip; the handler side keeps dropping bytes
	}
	rw.gz = gzr
	buf := make([]byte, 4096)
	for {
		n, err := gzr.Read(buf)
		if n > 0 {
			if _, ferr := rw.feed(buf[:n]); ferr != nil {
				return
			}
		}
		if err != nil {
			return // EOF (upstream finished) or corrupt stream: stop feeding
		}
	}
}

func (rw *responseWriter) feed(p []byte) (int, error) {
	events, err := rw.feeder.Write(p)
	if err != nil {
		return 0, err
	}
	if len(events) > 0 {
		if _, err := rw.w.Write(EncodeAll(events)); err != nil {
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
			if rw.gzSrc != nil {
				// Signal EOF to the pump goroutine and WAIT for it to drain
				// the last decompressed bytes into the feeder — the feeder
				// and rw.w are not goroutine-safe, so finish must not touch
				// them until the pump is done.
				_ = rw.gzSrc.Close()
				<-rw.gzDone
				rw.gzSrc = nil
			}
			events, err := rw.feeder.Close()
			if err == nil {
				_, _ = rw.w.Write(EncodeAll(events))
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
		var resp OpenAIResponse
		if err := json.Unmarshal(body, &resp); err == nil {
			out, _ = json.Marshal(TranslateResponse(&resp, rw.model))
		} else {
			status = http.StatusBadGateway
			out = TranslateError(status, body)
		}
	} else {
		out = TranslateError(status, body)
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
