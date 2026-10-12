package trace

import (
	"context"
	"net/http"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// TraceID returns the trace id shared by the handlers of one client
// request (minted by the first trace handler), or "" when none ran.
func TraceID(r *http.Request) string {
	id, _ := caddyhttp.GetVar(r.Context(), traceIDVar).(string)
	return id
}

// Recorder exposes the trace store to non-tracer modules that want to
// record their own exchanges (e.g. a handler that terminates requests
// itself, recording what it sends upstream). A Recorder is obtained from
// the llm_tracer app and is safe for concurrent use.
//
// It holds the store app, not its storage: handler Provision runs before
// the app's Start() opens the database, so the storage must be resolved
// per call (like the trace handler does).
type Recorder struct {
	app *Store
}

// NewRecorder returns a Recorder over the store app.
func (a *Store) NewRecorder() *Recorder { return &Recorder{app: a} }

// Start inserts the in-progress row for an exchange (write-ahead).
func (r *Recorder) Start(ctx context.Context, traceID, name string, ts time.Time) error {
	return r.app.Storage().StartRequest(ctx, traceID, name, ts)
}

// SaveRequest appends request-direction bytes to the raw log.
func (r *Recorder) SaveRequest(ctx context.Context, traceID, name string, p []byte) error {
	return r.app.Storage().Save(ctx, traceID, name, true, p)
}

// SaveResponse appends response-direction bytes to the raw log.
func (r *Recorder) SaveResponse(ctx context.Context, traceID, name string, p []byte) error {
	return r.app.Storage().Save(ctx, traceID, name, false, p)
}

// Record finalizes the exchange row. usage may be nil; input must not
// include cache tokens (normalize beforehand).
func (r *Recorder) Record(ctx context.Context, traceID, name string, ts time.Time, durMS, status, reqBytes, respBytes int, usage *Usage, model string) error {
	return r.app.Storage().RecordRequest(ctx, traceID, name, ts, durMS, status, reqBytes, respBytes, usage, model)
}
