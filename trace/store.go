// Package trace provides LLM request tracing for Caddy:
// storage for LLM request traces written by llm_tracer handlers, plus an HTTP
// query API handler module.
package trace

import (
	"bufio"
	"context"
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
)

func init() {
	caddy.RegisterModule(Store{})
	httpcaddyfile.RegisterGlobalOption("llm_tracer", parseGlobalOption)
	caddy.RegisterModule(TraceAPI{})
	httpcaddyfile.RegisterHandlerDirective("llm_traces_api", parseTraceAPICaddyfile)
}

// Store is the tracer app: a caddy.App that persists trace entries to an
// append-only JSONL file and serves queries.
type Store struct {
	// Dir is the directory holding traces.jsonl. Default: "llm-traces" in the
	// current working directory.
	Dir string `json:"dir,omitempty"`

	logger *zap.Logger
	disk   *diskStore
}

// CaddyModule returns the Caddy module information.
func (Store) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "llm.tracer",
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

// Start opens (or creates) the trace log and builds the in-memory index.
func (a *Store) Start() error {
	a.disk = newDiskStore(filepath.Join(a.Dir, "traces.jsonl"))
	return a.disk.open()
}

// Stop closes the trace log.
func (a *Store) Stop() error {
	if a.disk != nil {
		return a.disk.close()
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
func (a *Store) Storage() storage { return a.disk }

// Entry is one captured request/response exchange at one chain stage.
type Entry struct {
	ID              string      `json:"id"`
	Stage           string      `json:"stage"`
	Timestamp       time.Time   `json:"timestamp"`
	Method          string      `json:"method"`
	Path            string      `json:"path"`
	RequestHeaders  http.Header `json:"request_headers,omitempty"`
	RequestBody     []byte      `json:"request_body,omitempty"`
	Status          int         `json:"status,omitempty"`
	ResponseHeaders http.Header `json:"response_headers,omitempty"`
	ResponseBody    []byte      `json:"response_body,omitempty"`
	DurationMS      int64       `json:"duration_ms,omitempty"`
	Truncated       bool        `json:"truncated,omitempty"`
}

// EntrySummary is the list-view projection of an Entry (no bodies).
type EntrySummary struct {
	ID         string    `json:"id"`
	Stage      string    `json:"stage"`
	Timestamp  time.Time `json:"timestamp"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Status     int       `json:"status,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	ReqBytes   int       `json:"request_bytes"`
	RespBytes  int       `json:"response_bytes"`
}

// Query filters a List call.
type Query struct {
	Stage  string
	Limit  int
	Offset int
}

// Store persists and queries trace entries.
type storage interface {
	Append(ctx context.Context, e *Entry) error
	List(ctx context.Context, q Query) ([]EntrySummary, error)
	Get(ctx context.Context, id string) (*Entry, error)
}

// ---------- disk implementation ----------

type diskStore struct {
	path  string
	mu    sync.Mutex
	file  *os.File
	index []indexEntry // ordered by append time
	byID  map[string]int
}

type indexEntry struct {
	id     string
	stage  string
	offset int64
	size   int64
}

func newDiskStore(path string) *diskStore {
	return &diskStore{path: path, byID: map[string]int{}}
}

func (d *diskStore) open() error {
	if err := os.MkdirAll(filepath.Dir(d.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(d.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	d.file = f
	// Build index by scanning existing lines.
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 1024*1024), 256*1024*1024)
	var offset int64
	for scan.Scan() {
		n := int64(len(scan.Bytes())) + 1 // + newline
		var e struct {
			ID    string `json:"id"`
			Stage string `json:"stage"`
		}
		if err := json.Unmarshal(scan.Bytes(), &e); err == nil && e.ID != "" {
			d.index = append(d.index, indexEntry{id: e.ID, stage: e.Stage, offset: offset, size: n})
			d.byID[e.ID] = len(d.index) - 1
		}
		offset += n
	}
	return scan.Err()
}

func (d *diskStore) close() error {
	if d.file != nil {
		return d.file.Close()
	}
	return nil
}

func (d *diskStore) Append(_ context.Context, e *Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.file.Write(append(line, '\n')); err != nil {
		return err
	}
	// stat for offset would be racy; track from index
	var offset int64
	if n := len(d.index); n > 0 {
		last := d.index[n-1]
		offset = last.offset + last.size
	}
	d.index = append(d.index, indexEntry{id: e.ID, stage: e.Stage, offset: offset, size: int64(len(line)) + 1})
	d.byID[e.ID] = len(d.index) - 1
	return nil
}

func (d *diskStore) List(_ context.Context, q Query) ([]EntrySummary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []EntrySummary{}
	// Newest first.
	for i := len(d.index) - 1; i >= 0; i-- {
		ie := d.index[i]
		if q.Stage != "" && ie.stage != q.Stage {
			continue
		}
		if q.Offset > 0 {
			q.Offset--
			continue
		}
		e, err := d.readAt(ie)
		if err != nil {
			continue
		}
		out = append(out, EntrySummary{
			ID: e.ID, Stage: e.Stage, Timestamp: e.Timestamp, Method: e.Method,
			Path: e.Path, Status: e.Status, DurationMS: e.DurationMS,
			Truncated: e.Truncated,
			ReqBytes:  len(e.RequestBody), RespBytes: len(e.ResponseBody),
		})
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// Get returns one entry by ID.
func (d *diskStore) Get(_ context.Context, id string) (*Entry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i, ok := d.byID[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return d.readAt(d.index[i])
}

func (d *diskStore) readAt(ie indexEntry) (*Entry, error) {
	buf := make([]byte, ie.size)
	if _, err := d.file.ReadAt(buf, ie.offset); err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(trimNewline(buf), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

var _ storage = (*diskStore)(nil)

// ---------- query API handler ----------

// TraceAPI serves the trace query API: GET /llm/traces (list) and
// GET /llm/traces/{id} (full entry).
type TraceAPI struct {
	logger *zap.Logger
	app    *Store
}

// CaddyModule returns the Caddy module information.
func (TraceAPI) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.llm_traces_api",
		New: func() caddy.Module { return new(TraceAPI) },
	}
}

// Provision resolves the trace store app.
func (t *TraceAPI) Provision(ctx caddy.Context) error {
	t.logger = ctx.Logger()
	appIface, err := ctx.App("llm.tracer")
	if err != nil {
		return fmt.Errorf("llm_traces_api requires the llm_tracer global option: %w", err)
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
	path := strings.TrimPrefix(r.URL.Path, "/llm/traces")
	path = strings.Trim(path, "/")

	w.Header().Set("Content-Type", "application/json")

	if path == "" {
		q := Query{
			Stage:  r.URL.Query().Get("stage"),
			Limit:  intQuery(r, "limit", 100),
			Offset: intQuery(r, "offset", 0),
		}
		entries, err := t.app.Storage().List(r.Context(), q)
		if err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(entries)
	}

	e, err := t.app.Storage().Get(r.Context(), path)
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
		Name:  "llm.tracer",
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
