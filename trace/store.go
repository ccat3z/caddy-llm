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
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	_ "modernc.org/sqlite"
)

func init() {
	caddy.RegisterModule(Store{})
	caddy.RegisterModule(TraceAPI{})
}

// Store is the tracer app: a caddy.App persisting raw, replayable HTTP
// exchanges to rolling files with a SQLite index.
type Store struct {
	// Dir is the directory holding the raw history files and index.db.
	// Default: "llm-traces" in the current working directory.
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
		a.Dir = "llm-traces"
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
}

// RequestDetail is a full exchange: the aggregated metadata plus the raw,
// replayable HTTP message bytes of both directions.
type RequestDetail struct {
	RequestSummary
	Request  []byte `json:"request_raw"`  // full request message: request-line + headers + body
	Response []byte `json:"response_raw"` // full response message: status line + headers + body
}

// Query filters a List call.
type Query struct {
	Name   string
	Limit  int
	Offset int
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
	RecordRequest(ctx context.Context, traceID, name string, ts time.Time, durMS, status, reqBytes, respBytes int) error

	List(ctx context.Context, q Query) ([]RequestSummary, error)
	Get(ctx context.Context, traceID, name string) (*RequestDetail, error)
}

// ---------- raw file + sqlite index implementation ----------

// rotateSize is the size threshold at which a new history file is opened.
const rotateSize = 100 << 20 // 100MB

// rawStore appends raw message bytes to rolling history-<ts>.raw files and
// tracks every segment in SQLite.
type rawStore struct {
	dir string

	mu sync.Mutex
	db *sql.DB

	// active is the file currently appended to.
	active     *os.File
	activeName string
	// activeSize is its current byte size (tracked, not stat'ed).
	activeSize int64
	// segments maps a logical exchange direction to its open raw_log_idx
	// row, so repeated Saves continue the same segment.
	segments map[segmentKey]segmentPos
	// readers caches read handles per history file.
	readers map[string]*os.File
}

// rawPiece locates one physical segment of a message.
type rawPiece struct {
	file         string
	offset, size int64
	isReq        bool
}

type segmentKey struct {
	traceID, name string
	isReq         bool
}

type segmentPos struct {
	idxID  int64 // raw_log_idx.id
	offset int64 // bytes already written for this segment
}

func newRawStore(dir string) *rawStore {
	return &rawStore{
		dir:      dir,
		segments: map[segmentKey]segmentPos{},
		readers:  map[string]*os.File{},
	}
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
    resp_bytes  INTEGER
);
CREATE INDEX IF NOT EXISTS idx_requests_name ON llm_requests(trace_name);`
	_, err := s.db.Exec(schema)
	return err
}

func (s *rawStore) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	if s.active != nil {
		firstErr = s.active.Close()
		s.active = nil
	}
	for _, f := range s.readers {
		f.Close()
	}
	s.readers = nil
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

	key := segmentKey{traceID, name, isReq}
	seg, continuing := s.segments[key]

	// Rotate when the active file exceeds the threshold; the in-flight
	// segment is sealed and continues as a new row in the fresh file.
	if s.activeSize > rotateSize {
		if continuing {
			// seal the current row; a new row opens below for the remainder
			delete(s.segments, key)
			continuing = false
		}
		if err := s.rotate(); err != nil {
			return err
		}
	}

	segStart := s.activeSize // segment starts at the current end of file
	if _, err := s.active.Write(p); err != nil {
		return err
	}
	s.activeSize += int64(len(p))

	if !continuing {
		// A fresh segment (rotated continuation or first write): its size
		// counter starts over; sealed rows keep their own totals.
		seg = segmentPos{}
	}
	seg.offset += int64(len(p))
	s.segments[key] = seg // keep the write point current for the next Save

	if !continuing {
		res, err := s.db.Exec(
			`INSERT INTO raw_log_idx (trace_id, trace_name, ts, file, offset, size, is_req) VALUES (?,?,?,?,?,?,?)`,
			traceID, name, time.Now().UTC(), s.activeName, segStart, seg.offset, boolInt(isReq))
		if err != nil {
			return err
		}
		idxID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		seg.idxID = idxID
		s.segments[key] = seg
		return nil
	}
	_, err := s.db.Exec(`UPDATE raw_log_idx SET size=? WHERE id=?`, seg.offset, seg.idxID)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RecordRequest implements storage.
func (s *rawStore) RecordRequest(_ context.Context, traceID, name string, ts time.Time, durMS, status, reqBytes, respBytes int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO llm_requests (trace_id, trace_name, ts, dur_ms, status, req_bytes, resp_bytes) VALUES (?,?,?,?,?,?,?)`,
		traceID, name, ts.UTC(), durMS, status, reqBytes, respBytes)
	return err
}

// List implements storage.
func (s *rawStore) List(_ context.Context, q Query) ([]RequestSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `SELECT trace_id, trace_name, ts, dur_ms, status, req_bytes, resp_bytes FROM llm_requests`
	args := []any{}
	if q.Name != "" {
		query += ` WHERE trace_name = ?`
		args = append(args, q.Name)
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
		if err := rows.Scan(&r.TraceID, &r.TraceName, &r.Timestamp, &r.DurationMS, &r.Status, &r.ReqBytes, &r.RespBytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get implements storage.
func (s *rawStore) Get(_ context.Context, traceID, name string) (*RequestDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	det := &RequestDetail{}
	// Aggregate row (may be missing for incomplete streams).
	aggErr := s.db.QueryRow(
		`SELECT trace_id, trace_name, ts, dur_ms, status, req_bytes, resp_bytes FROM llm_requests
		 WHERE trace_id=? AND trace_name=? ORDER BY id DESC LIMIT 1`, traceID, name).
		Scan(&det.TraceID, &det.TraceName, &det.Timestamp, &det.DurationMS, &det.Status, &det.ReqBytes, &det.RespBytes)
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

// readParts concatenates the raw bytes of the given segments. Callers hold s.mu.
func (s *rawStore) readParts(parts []rawPiece) ([]byte, error) {
	if len(parts) == 0 {
		return nil, nil
	}
	var out []byte
	for _, p := range parts {
		f, err := s.reader(p.file)
		if err != nil {
			if os.IsNotExist(err) {
				continue // externally deleted history file: skip its bytes
			}
			return nil, err
		}
		buf := make([]byte, p.size)
		if _, err := f.ReadAt(buf, p.offset); err != nil {
			return nil, err
		}
		out = append(out, buf...)
	}
	return out, nil
}

// reader returns a cached read handle for a history file. Callers hold s.mu.
func (s *rawStore) reader(name string) (*os.File, error) {
	if f, ok := s.readers[name]; ok {
		return f, nil
	}
	f, err := os.Open(filepath.Join(s.dir, name))
	if err != nil {
		return nil, err
	}
	s.readers[name] = f
	return f, nil
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
			Name:   r.URL.Query().Get("stage"),
			Limit:  intQuery(r, "limit", 100),
			Offset: intQuery(r, "offset", 0),
		}
		entries, err := t.app.Storage().List(r.Context(), q)
		if err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(entries)
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
