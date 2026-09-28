// Package trace provides LLM request tracing for Caddy:
// storage for LLM request traces written by trace handlers, plus an HTTP
// query API handler module.
package trace

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	_ "modernc.org/sqlite"
)

func init() {
	caddy.RegisterModule(Store{})
	httpcaddyfile.RegisterGlobalOption("llm_tracer", parseGlobalOption)
	caddy.RegisterModule(TraceAPI{})
	httpcaddyfile.RegisterHandlerDirective("llm_tracer_api", parseTraceAPICaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("llm_tracer_api", httpcaddyfile.Before, "respond")
}

// Store is the tracer app: a caddy.App persisting raw, replayable HTTP
// exchanges to rolling files with a SQLite index.
type Store struct {
	// Dir is the directory holding the raw history files and index.db.
	// Required — the trace data is the point of running this app, so where
	// it lives must be a deliberate choice, never a CWD accident.
	Dir string `json:"dir,omitempty"`

	logger *zap.Logger
	db     *rawStore
}

// CaddyModule returns the Caddy module information.
func (Store) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "llm_tracer",
		New: func() caddy.Module { return new(Store) },
	}
}

// Provision sets up the module.
func (a *Store) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger()
	if a.Dir == "" {
		return fmt.Errorf(`llm_tracer requires a storage dir ("dir" in JSON, or the llm_tracer global option's argument in a Caddyfile)`)
	}
	return nil
}

// Start opens the index database and a fresh history file.
func (a *Store) Start() error {
	a.db = newRawStore(a.Dir)
	return a.db.open()
}

// Stop closes the store.
func (a *Store) Stop() error {
	if a.db != nil {
		return a.db.close()
	}
	return nil
}

// Interface guards
var (
	_ caddy.Provisioner = (*Store)(nil)
	_ caddy.App         = (*Store)(nil)
)

// Storage returns the underlying storage implementation (used by the tracer
// handler and the traces API).
func (a *Store) Storage() storage { return a.db }

// RequestSummary is the list-view projection of one completed traced
// exchange (one row of llm_requests).
type RequestSummary struct {
	TraceID    string    `json:"trace_id"`
	TraceName  string    `json:"trace_name"`
	Timestamp  time.Time `json:"timestamp"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Status     int       `json:"status,omitempty"`
	ReqBytes   int       `json:"req_bytes"`
	RespBytes  int       `json:"resp_bytes"`
	// Token accounting, normalized so the three are independent (input
	// never includes cache). HasUsage is false for exchanges whose
	// response carried no usage (errors, probes, truncated streams).
	InputTokens  int  `json:"input_tokens,omitempty"`
	CacheTokens  int  `json:"cache_tokens,omitempty"`
	OutputTokens int  `json:"output_tokens,omitempty"`
	HasUsage     bool `json:"has_usage,omitempty"`
}

// RequestDetail is a full exchange: the aggregated metadata plus the raw,
// replayable HTTP message bytes of both directions.
type RequestDetail struct {
	RequestSummary
	Request  []byte `json:"request_raw"`  // full request message: request-line + headers + body
	Response []byte `json:"response_raw"` // full response message: status line + headers + body
}

// UsageBucket is one interval of the usage time series.
type UsageBucket struct {
	Timestamp time.Time `json:"timestamp"`
	Input     int64     `json:"input_tokens"`
	Cache     int64     `json:"cache_tokens"`
	Output    int64     `json:"output_tokens"`
	Requests  int64     `json:"requests"`
}

// Query filters a List call.
type Query struct {
	TraceName string
	Limit     int
	Offset    int
}

// storage persists and queries traced exchanges. Raw message bytes are
// appended to rolling history files; all positioning and aggregate metadata
// live in SQLite.
type storage interface {
	// Save appends raw message bytes (any part of an HTTP message: the
	// request/status line, headers, blank line, or body chunks) for one
	// direction of one traced exchange. Repeated Saves with the same
	// (traceID, name, isReq) continue the same on-disk segment; the first
	// Save opens a raw_log_idx row, later ones only grow its size.
	Save(ctx context.Context, traceID, name string, isReq bool, p []byte) error

	// RecordRequest writes the aggregate row for a completed exchange.
	// usage is nil when the response carried no token accounting.
	RecordRequest(ctx context.Context, traceID, name string, ts time.Time, durMS, status, reqBytes, respBytes int, usage *Usage) error

	List(ctx context.Context, q Query) ([]RequestSummary, error)
	Get(ctx context.Context, traceID, name string) (*RequestDetail, error)

	// UsageSeries aggregates token usage into time buckets of interval
	// seconds, optionally filtered by trace name and time range (zero times =
	// unbounded).
	UsageSeries(ctx context.Context, intervalSec int64, from, to time.Time, traceName string) ([]UsageBucket, error)
}

// ---------- raw file + sqlite index implementation ----------

// rotateSize is the size threshold at which a new history file is opened.
const rotateSize = 100 << 20 // 100MB

// rawStore appends raw message bytes to rolling history-<ts>.raw files and
// tracks every write in SQLite: one row per physical write. A logical
// message (one direction of one exchange) is any number of rows; Get
// concatenates them in id order — concurrent streams interleaving in the
// same file each keep their own bytes, and rotation needs no coordination.
type rawStore struct {
	dir string

	// mu guards the write path and the active-file bookkeeping; readers
	// (List/Get) take it shared so a trace query never blocks in-flight
	// streams (and vice versa on the metadata queries).
	mu sync.RWMutex
	db *sql.DB

	// active is the file currently appended to — the only held-open read
	// handle. Historical files are opened transiently on Get.
	active     *os.File
	activeName string
	// activeSize is its current byte size (tracked, not stat'ed).
	activeSize int64
}

// rawPiece locates one physical segment of a message.
type rawPiece struct {
	file         string
	offset, size int64
	isReq        bool
}

func newRawStore(dir string) *rawStore {
	return &rawStore{dir: dir}
}

func (s *rawStore) open() error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(s.dir, "index.db"))
	if err != nil {
		return err
	}
	// WAL keeps queries from blocking the write path.
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=NORMAL`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return err
		}
	}
	s.db = db
	if err := s.createTables(); err != nil {
		return err
	}
	return s.rotate() // open the first active file
}

func (s *rawStore) createTables() error {
	const schema = `
CREATE TABLE IF NOT EXISTS raw_log_idx (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    trace_id    TEXT,
    trace_name  TEXT,
    ts          TIMESTAMP,
    file        TEXT,
    offset      INTEGER,
    size        INTEGER,
    is_req      INTEGER
);
CREATE INDEX IF NOT EXISTS idx_raw_trace ON raw_log_idx(trace_id, trace_name);
CREATE TABLE IF NOT EXISTS llm_requests (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    trace_id    TEXT,
    trace_name  TEXT,
    ts          TIMESTAMP,
    dur_ms      INTEGER,
    status      INTEGER,
    req_bytes   INTEGER,
    resp_bytes  INTEGER,
    in_tokens   INTEGER,
    cache_tokens INTEGER,
    out_tokens  INTEGER
);
CREATE INDEX IF NOT EXISTS idx_requests_name ON llm_requests(trace_name);`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Migrate pre-token-stats databases: add the columns if missing.
	// (Exchanges recorded before this have NULL usage — no usage detected.)
	for _, col := range []string{"in_tokens", "cache_tokens", "out_tokens"} {
		if _, err := s.db.Exec(`ALTER TABLE llm_requests ADD COLUMN ` + col + ` INTEGER`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

func (s *rawStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	if s.active != nil {
		firstErr = s.active.Close()
		s.active = nil
	}
	if err := s.db.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// rotate closes the active file and opens a fresh one. Callers hold s.mu.
func (s *rawStore) rotate() error {
	if s.active != nil {
		if err := s.active.Close(); err != nil {
			return err
		}
		s.active = nil
	}
	name := fmt.Sprintf("history-%d.raw", time.Now().UnixNano())
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.active = f
	s.activeName = name
	s.activeSize = 0
	return nil
}

// Save implements storage.
func (s *rawStore) Save(_ context.Context, traceID, name string, isReq bool, p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Rotate when the active file exceeds the threshold; the next write
	// simply lands in the fresh file. No open-segment bookkeeping: every
	// write is its own row, so nothing needs sealing.
	if s.activeSize > rotateSize {
		if err := s.rotate(); err != nil {
			return err
		}
	}

	offset := s.activeSize
	if _, err := s.active.Write(p); err != nil {
		return err
	}
	s.activeSize += int64(len(p))

	_, err := s.db.Exec(
		`INSERT INTO raw_log_idx (trace_id, trace_name, ts, file, offset, size, is_req) VALUES (?,?,?,?,?,?,?)`,
		traceID, name, time.Now().UTC(), s.activeName, offset, len(p), boolInt(isReq))
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RecordRequest implements storage.
func (s *rawStore) RecordRequest(_ context.Context, traceID, name string, ts time.Time, durMS, status, reqBytes, respBytes int, usage *Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var in, cache, out any
	if usage != nil {
		in, cache, out = usage.Input, usage.Cache, usage.Output
	}
	_, err := s.db.Exec(
		`INSERT INTO llm_requests (trace_id, trace_name, ts, dur_ms, status, req_bytes, resp_bytes, in_tokens, cache_tokens, out_tokens) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		traceID, name, ts.UTC(), durMS, status, reqBytes, respBytes, in, cache, out)
	return err
}

// summaryColumns is the SELECT projection shared by List and Get.
const summaryColumns = `trace_id, trace_name, ts, dur_ms, status, req_bytes, resp_bytes, in_tokens, cache_tokens, out_tokens`

// scanSummary scans one llm_requests row (summaryColumns order) into a
// RequestSummary. NULL token columns mean no usage was detected.
func scanSummary(row interface{ Scan(...any) error }, r *RequestSummary) error {
	var in, cache, out sql.NullInt64
	if err := row.Scan(&r.TraceID, &r.TraceName, &r.Timestamp, &r.DurationMS, &r.Status, &r.ReqBytes, &r.RespBytes, &in, &cache, &out); err != nil {
		return err
	}
	r.HasUsage = in.Valid || cache.Valid || out.Valid
	r.InputTokens, r.CacheTokens, r.OutputTokens = int(in.Int64), int(cache.Int64), int(out.Int64)
	return nil
}

// List implements storage.
func (s *rawStore) List(_ context.Context, q Query) ([]RequestSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT ` + summaryColumns + ` FROM llm_requests`
	args := []any{}
	if q.TraceName != "" {
		query += ` WHERE trace_name = ?`
		args = append(args, q.TraceName)
	}
	query += ` ORDER BY id DESC`
	if q.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, q.Limit, q.Offset)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RequestSummary{}
	for rows.Next() {
		var r RequestSummary
		if err := scanSummary(rows, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get implements storage.
func (s *rawStore) Get(_ context.Context, traceID, name string) (*RequestDetail, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	det := &RequestDetail{}
	// Aggregate row (may be missing for incomplete streams).
	aggErr := scanSummary(s.db.QueryRow(
		`SELECT `+summaryColumns+` FROM llm_requests
		 WHERE trace_id=? AND trace_name=? ORDER BY id DESC LIMIT 1`, traceID, name), &det.RequestSummary)
	if aggErr != nil && aggErr != sql.ErrNoRows {
		return nil, aggErr
	}

	rows, err := s.db.Query(
		`SELECT file, offset, size, is_req FROM raw_log_idx
		 WHERE trace_id=? AND trace_name=? ORDER BY id`, traceID, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reqParts, respParts []rawPiece
	for rows.Next() {
		var p rawPiece
		var isReq int
		if err := rows.Scan(&p.file, &p.offset, &p.size, &isReq); err != nil {
			return nil, err
		}
		p.isReq = isReq == 1
		if p.isReq {
			reqParts = append(reqParts, p)
		} else {
			respParts = append(respParts, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if aggErr == sql.ErrNoRows && len(reqParts) == 0 && len(respParts) == 0 {
		return nil, os.ErrNotExist
	}

	if det.Request, err = s.readParts(reqParts); err != nil {
		return nil, err
	}
	if det.Response, err = s.readParts(respParts); err != nil {
		return nil, err
	}
	return det, nil
}

// UsageSeries implements storage. Bucketing happens in SQL on unix seconds
// (strftime parses the RFC3339 ts), so bucket edges are interval-aligned and
// empty buckets are simply absent.
func (s *rawStore) UsageSeries(_ context.Context, intervalSec int64, from, to time.Time, traceName string) ([]UsageBucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if intervalSec <= 0 {
		intervalSec = 3600
	}
	var (
		where []string
		args  []any
	)
	if traceName != "" {
		where = append(where, `trace_name = ?`)
		args = append(args, traceName)
	}
	if !from.IsZero() {
		where = append(where, `ts >= ?`)
		args = append(args, from.UTC())
	}
	if !to.IsZero() {
		where = append(where, `ts < ?`)
		args = append(args, to.UTC())
	}
	// ts is stored RFC3339 UTC ("...T10:05:00Z"); SQLite's strftime can't
	// parse the trailing Z, so bucket on the first 19 chars.
	query := `SELECT (CAST(strftime('%s', substr(ts, 1, 19)) AS INTEGER) / ?) * ? AS bucket,
		COALESCE(SUM(in_tokens), 0), COALESCE(SUM(cache_tokens), 0), COALESCE(SUM(out_tokens), 0), COUNT(*)
		FROM llm_requests`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` GROUP BY bucket ORDER BY bucket`
	args = append([]any{intervalSec, intervalSec}, args...)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageBucket{}
	for rows.Next() {
		var b UsageBucket
		var bucket int64
		if err := rows.Scan(&bucket, &b.Input, &b.Cache, &b.Output, &b.Requests); err != nil {
			return nil, err
		}
		b.Timestamp = time.Unix(bucket, 0).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

// readParts concatenates the raw bytes of the given segments. Every history
// file is opened transiently — the only held-open handle is the active
// write file, and it is O_WRONLY — so reads never accumulate fds. Callers
// hold s.mu (shared).
func (s *rawStore) readParts(parts []rawPiece) ([]byte, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	var out []byte
	var cur *os.File
	curName := ""
	defer func() {
		if cur != nil {
			cur.Close()
		}
	}()
	for _, p := range parts {
		if cur == nil || p.file != curName {
			if cur != nil {
				cur.Close()
			}
			f, err := os.Open(filepath.Join(s.dir, p.file))
			if err != nil {
				if os.IsNotExist(err) {
					cur, curName = nil, "" // externally deleted history file: skip its bytes
					continue
				}
				return nil, err
			}
			cur, curName = f, p.file
		}
		buf := make([]byte, p.size)
		if _, err := cur.ReadAt(buf, p.offset); err != nil {
			return nil, err
		}
		out = append(out, buf...)
	}
	return out, nil
}

var _ storage = (*rawStore)(nil)

// ---------- query API handler ----------

// TraceAPI serves the trace query API on whatever path the route is mounted
// at (the prefix is stripped upstream, e.g. by rewrite's strip_path_prefix):
// "" → list, "{traceID}/{name}" → one exchange.
type TraceAPI struct {
	logger *zap.Logger
	app    *Store
}

// CaddyModule returns the Caddy module information.
func (TraceAPI) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.llm_tracer_api",
		New: func() caddy.Module { return new(TraceAPI) },
	}
}

// Provision resolves the trace store app.
func (t *TraceAPI) Provision(ctx caddy.Context) error {
	t.logger = ctx.Logger()
	appIface, err := ctx.App("llm_tracer")
	if err != nil {
		return fmt.Errorf("llm_tracer_api requires the llm_tracer app: %w", err)
	}
	t.app = appIface.(*Store)
	return nil
}

// Interface guard
var _ caddyhttp.MiddlewareHandler = (*TraceAPI)(nil)

func (t *TraceAPI) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if r.Method != http.MethodGet {
		return next.ServeHTTP(w, r)
	}
	// The route prefix (e.g. /llm/traces) is stripped before this handler;
	// what remains is "" (list) or "{traceID}/{name}".
	rest := strings.Trim(r.URL.Path, "/")

	w.Header().Set("Content-Type", "application/json")

	if rest == "" {
		q := Query{
			TraceName: r.URL.Query().Get("trace_name"),
			Limit:     intQuery(r, "limit", 100),
			Offset:    intQuery(r, "offset", 0),
		}
		entries, err := t.app.Storage().List(r.Context(), q)
		if err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(entries)
	}

	// Usage time series: /usage?interval=1h&from=&to=&trace_name=
	if rest == "usage" {
		interval := int64(durationQuery(r, "interval", time.Hour).Seconds())
		from, _ := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
		to, _ := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
		buckets, err := t.app.Storage().UsageSeries(r.Context(), interval, from, to, r.URL.Query().Get("trace_name"))
		if err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(buckets)
	}

	// The embedded dashboard: /ui and /ui/*.
	if rest == "ui" || strings.HasPrefix(rest, uiPrefix) {
		t.serveUI(w, r, rest)
		return nil
	}

	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"expected /{traceID}/{traceName}"}`))
		return nil
	}
	e, err := t.app.Storage().Get(r.Context(), parts[0], parts[1])
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
		return nil
	}
	return json.NewEncoder(w).Encode(e)
}

func intQuery(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// durationQuery parses a duration query param, accepting Go duration strings
// ("1h", "30m") or bare seconds ("900").
func durationQuery(r *http.Request, name string, def time.Duration) time.Duration {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

// ---------- Caddyfile ----------

func parseGlobalOption(d *caddyfile.Dispenser, _ any) (any, error) {
	app := new(Store)
	for d.Next() {
		if d.NextArg() {
			app.Dir = d.Val()
		}
		for d.NextBlock(0) {
			switch d.Val() {
			case "dir":
				if !d.NextArg() {
					return nil, d.ArgErr()
				}
				app.Dir = d.Val()
			default:
				return nil, d.Errf("unknown subdirective %q", d.Val())
			}
		}
	}
	return httpcaddyfile.App{
		Name:  "llm_tracer",
		Value: caddyconfig.JSON(app, nil),
	}, nil
}

func parseTraceAPICaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var t TraceAPI
	for h.Next() {
		if h.NextArg() {
			return nil, h.ArgErr()
		}
	}
	return &t, nil
}
