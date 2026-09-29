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
	// MaxSize caps the total size of the trace directory: when exceeded,
	// the oldest history files are deleted (their raw_log_idx rows too;
	// llm_requests summaries stay, so token stats survive). Accepts a byte
	// count or a size string ("10G", "512M"); 0 disables cleanup.
	// Default: 10G.
	MaxSize json.RawMessage `json:"max_size,omitempty"`

	logger  *zap.Logger
	db      *rawStore
	dir     string
	maxSize int64
}

// defaultMaxSize is the cleanup threshold when max_size isn't configured.
const defaultMaxSize = 10 << 30 // 10 GiB

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
	a.dir = filepath.Join(caddy.AppDataDir(), "llm-tracer")
	a.maxSize = defaultMaxSize
	if len(a.MaxSize) > 0 {
		ms, err := parseByteSize(a.MaxSize)
		if err != nil {
			return fmt.Errorf("max_size: %v", err)
		}
		a.maxSize = ms
	}
	return nil
}

// parseByteSize accepts a JSON string ("10G", "512M", "1GiB", case
// insensitive) or a plain JSON number (bytes). Sizes are 1024-based.
func parseByteSize(raw json.RawMessage) (int64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil {
			return 0, fmt.Errorf("must be a size string or a byte count")
		}
		if n < 0 {
			return 0, fmt.Errorf("must not be negative")
		}
		return n, nil
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q", s)
	}
	var mult float64 = 1
	switch strings.ToUpper(strings.TrimSpace(s[i:])) {
	case "", "B":
	case "K", "KB", "KIB":
		mult = 1 << 10
	case "M", "MB", "MIB":
		mult = 1 << 20
	case "G", "GB", "GIB":
		mult = 1 << 30
	case "T", "TB", "TIB":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown size suffix in %q", s)
	}
	return int64(num * mult), nil
}

// Start opens the index database and a fresh history file.
func (a *Store) Start() error {
	a.db = newRawStore(a.dir)
	a.db.maxSize = a.maxSize
	a.db.logger = a.logger
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

// RequestSummary is the list-view projection of one traced exchange (one
// row of llm_requests).
type RequestSummary struct {
	TraceID    string    `json:"trace_id"`
	TraceName  string    `json:"trace_name"`
	Timestamp  time.Time `json:"timestamp"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Status     int       `json:"status,omitempty"`
	ReqBytes   int       `json:"req_bytes"`
	RespBytes  int       `json:"resp_bytes"`
	// State distinguishes in-flight and crashed rows from completed ones:
	// "in_progress" (started, not yet finished) or "crashed" (was in
	// progress when the process died, marked at store open). Empty means
	// the exchange completed normally (possibly with an abort status).
	State string `json:"state,omitempty"`
	// Model is the request body's top-level "model" field (empty for
	// non-LLM or malformed bodies, and while in progress — it's set when
	// the row is finalized, like the other completion columns).
	Model string `json:"model,omitempty"`
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

	// StartRequest inserts the aggregate row for a just-started exchange
	// (state in_progress), so in-flight requests are queryable and a crash
	// leaves a visible row instead of nothing.
	StartRequest(ctx context.Context, traceID, name string, ts time.Time) error

	// RecordRequest finalizes the row for a completed exchange (state
	// completed). usage is nil when the response carried no token
	// accounting. Falls back to an insert when StartRequest never ran.
	RecordRequest(ctx context.Context, traceID, name string, ts time.Time, durMS, status, reqBytes, respBytes int, usage *Usage, model string) error

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

	// maxSize caps the directory's total size; 0 disables cleanup.
	maxSize int64
	logger  *zap.Logger
	stopCh  chan struct{}
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
	if err := s.markStaleCrashed(); err != nil {
		return err
	}
	if err := s.rotate(); err != nil { // open the first active file
		return err
	}
	if s.maxSize > 0 {
		s.stopCh = make(chan struct{})
		go s.cleanupLoop()
	}
	return nil
}

// cleanupLoop enforces maxSize once at start and then every minute.
func (s *rawStore) cleanupLoop() {
	s.cleanup()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.cleanup()
		}
	}
}

// cleanup deletes the oldest history files (never the active one) until
// the directory is under maxSize. Their raw_log_idx rows go with them;
// llm_requests summaries are kept — token stats outlive raw bodies, and
// Get already tolerates missing history files.
func (s *rawStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	type hist struct {
		name string
		size int64
	}
	var (
		files []hist
		total int64
	)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		if strings.HasPrefix(e.Name(), "history-") {
			files = append(files, hist{e.Name(), info.Size()})
		}
	}
	// File names carry a nanosecond timestamp, so name order is age order.
	for len(files) > 1 && total > s.maxSize {
		oldest := files[0]
		if oldest.name == s.activeName {
			break
		}
		if err := os.Remove(filepath.Join(s.dir, oldest.name)); err != nil {
			continue
		}
		if _, err := s.db.Exec(`DELETE FROM raw_log_idx WHERE file = ?`, oldest.name); err != nil && s.logger != nil {
			s.logger.Warn("cleanup: prune index", zap.String("file", oldest.name), zap.Error(err))
		}
		if s.logger != nil {
			s.logger.Info("cleanup: deleted history file",
				zap.String("file", oldest.name), zap.Int64("size", oldest.size), zap.Int64("total_before", total))
		}
		total -= oldest.size
		files = files[1:]
	}
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
    model       TEXT,
    in_tokens   INTEGER,
    cache_tokens INTEGER,
    out_tokens  INTEGER,
    done        INTEGER
);
CREATE INDEX IF NOT EXISTS idx_requests_name ON llm_requests(trace_name);`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	// Migrate pre-token-stats databases: add the columns if missing.
	// (Exchanges recorded before this have NULL usage — no usage detected.)
	for _, col := range []string{"in_tokens", "cache_tokens", "out_tokens", "done"} {
		if _, err := s.db.Exec(`ALTER TABLE llm_requests ADD COLUMN ` + col + ` INTEGER`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	if _, err := s.db.Exec(`ALTER TABLE llm_requests ADD COLUMN model TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	return nil
}

// done states for llm_requests.done.
const (
	doneInProgress = 0
	doneCompleted  = 1
	doneCrashed    = 2
)

// markStaleCrashed flips every in-progress row to crashed. The store is
// single-writer per process, so any in-progress row at open time belongs
// to a process that died mid-exchange.
func (s *rawStore) markStaleCrashed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE llm_requests SET done=? WHERE done=?`, doneCrashed, doneInProgress)
	return err
}

func (s *rawStore) close() error {
	if s.stopCh != nil {
		close(s.stopCh)
		s.stopCh = nil
	}
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

// StartRequest implements storage.
func (s *rawStore) StartRequest(_ context.Context, traceID, name string, ts time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO llm_requests (trace_id, trace_name, ts, done) VALUES (?,?,?,?)`,
		traceID, name, ts.UTC(), doneInProgress)
	return err
}

// RecordRequest implements storage.
func (s *rawStore) RecordRequest(_ context.Context, traceID, name string, ts time.Time, durMS, status, reqBytes, respBytes int, usage *Usage, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var in, cache, out any
	if usage != nil {
		in, cache, out = usage.Input, usage.Cache, usage.Output
	}
	res, err := s.db.Exec(
		`UPDATE llm_requests SET dur_ms=?, status=?, req_bytes=?, resp_bytes=?, model=?, in_tokens=?, cache_tokens=?, out_tokens=?, done=?
		 WHERE trace_id=? AND trace_name=? AND done=?`,
		durMS, status, reqBytes, respBytes, model, in, cache, out, doneCompleted,
		traceID, name, doneInProgress)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	// StartRequest never ran (e.g. storage was broken at request start):
	// fall back to a direct insert of the completed row.
	_, err = s.db.Exec(
		`INSERT INTO llm_requests (trace_id, trace_name, ts, dur_ms, status, req_bytes, resp_bytes, model, in_tokens, cache_tokens, out_tokens, done) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		traceID, name, ts.UTC(), durMS, status, reqBytes, respBytes, model, in, cache, out, doneCompleted)
	return err
}

// summaryColumns is the SELECT projection shared by List and Get.
const summaryColumns = `trace_id, trace_name, ts, dur_ms, status, req_bytes, resp_bytes, in_tokens, cache_tokens, out_tokens, done, model`

// scanSummary scans one llm_requests row (summaryColumns order) into a
// RequestSummary. Completion columns are NULL while in progress; NULL
// token columns mean no usage was detected; NULL done
// (pre-in-progress-tracking rows) means completed; NULL model means the
// request body carried no model (or the row predates model tracking).
func scanSummary(row interface{ Scan(...any) error }, r *RequestSummary) error {
	var dur, status, reqB, respB, in, cache, out, done sql.NullInt64
	var model sql.NullString
	if err := row.Scan(&r.TraceID, &r.TraceName, &r.Timestamp, &dur, &status, &reqB, &respB, &in, &cache, &out, &done, &model); err != nil {
		return err
	}
	r.Model = model.String
	r.DurationMS = dur.Int64
	r.Status = int(status.Int64)
	r.ReqBytes = int(reqB.Int64)
	r.RespBytes = int(respB.Int64)
	r.HasUsage = in.Valid || cache.Valid || out.Valid
	r.InputTokens, r.CacheTokens, r.OutputTokens = int(in.Int64), int(cache.Int64), int(out.Int64)
	switch {
	case done.Valid && done.Int64 == doneInProgress:
		r.State = "in_progress"
	case done.Valid && done.Int64 == doneCrashed:
		r.State = "crashed"
	}
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
	// Raw download: ?part=request|response serves one direction's raw
	// message bytes as an attachment (right-clickable link from the UI).
	if part := r.URL.Query().Get("part"); part != "" {
		var raw []byte
		switch part {
		case "request":
			raw = e.Request
		case "response":
			raw = e.Response
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"part must be request or response"}`))
			return nil
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=%q", parts[0]+"-"+parts[1]+"-"+part+".log"))
		_, _ = w.Write(raw)
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
			return nil, d.ArgErr()
		}
		for d.NextBlock(0) {
			switch d.Val() {
			case "max_size":
				if !d.NextArg() {
					return nil, d.ArgErr()
				}
				app.MaxSize = json.RawMessage(strconv.Quote(d.Val()))
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
