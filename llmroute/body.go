package llmroute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// ReadOnlyJsonBody is an immutable, parsed JSON request body. Obj and the
// wire bytes never change after construction; the wire form is lazy — it is
// marshaled from Obj once, on first consumption (Read, Marshal, or GetBody),
// then cached. The lazy initialization assumes a single consuming goroutine,
// the same contract http request bodies have.
//
// To "modify" a body, build a new one — from a shallow or deep copy of Obj,
// whichever the change needs — and install it with SetRequestBody. The
// request's Body is the single source of truth: a fallthrough always sees
// the last value installed, never a mutated alias, so sharing one body
// across attempts is safe and copies are only needed where a value actually
// changes.
type ReadOnlyJsonBody struct {
	// Obj is the parsed JSON object. Immutable by convention from
	// construction on: NewJsonBody takes ownership, so callers must not mutate it
	// (at any depth) after handing it over.
	Obj map[string]any

	// raw is the lazy wire form: nil until first needed, then fixed.
	raw []byte
	off int
	eof bool
}

// NewJsonBodyFromBytes parses raw as a JSON object.
func NewJsonBodyFromBytes(raw []byte) (*ReadOnlyJsonBody, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if obj == nil { // "null" unmarshals to a nil map without error
		return nil, io.ErrUnexpectedEOF
	}
	return NewJsonBody(obj), nil
}

// NewJsonBody wraps an already-parsed JSON object. The map is owned by the
// body from here on; callers must not retain or mutate it.
func NewJsonBody(obj map[string]any) *ReadOnlyJsonBody { return &ReadOnlyJsonBody{Obj: obj} }

// Marshal returns the body's wire bytes, marshaling them from Obj if not
// already cached. A pure snapshot: it does not disturb the read cursor, and
// caching has no observable effect since Obj never changes.
func (b *ReadOnlyJsonBody) Marshal() ([]byte, error) {
	if err := b.marshal(); err != nil {
		return nil, err
	}
	return b.raw, nil
}

// marshal caches the wire form from Obj.
func (b *ReadOnlyJsonBody) marshal() error {
	if b.raw != nil {
		return nil
	}
	raw, err := json.Marshal(b.Obj)
	if err != nil {
		return err
	}
	b.raw = raw
	return nil
}

// Read streams the wire bytes; the first call marshals and caches them.
func (b *ReadOnlyJsonBody) Read(p []byte) (int, error) {
	if b.eof {
		return 0, io.EOF
	}
	if b.raw == nil {
		if err := b.marshal(); err != nil {
			return 0, err
		}
	}
	n := copy(p, b.raw[b.off:])
	b.off += n
	if b.off >= len(b.raw) {
		b.eof = true
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// Close is a no-op; the bytes live in memory.
func (b *ReadOnlyJsonBody) Close() error { return nil }

var _ io.ReadCloser = (*ReadOnlyJsonBody)(nil)

// FromBody converts the request's body into its parsed form: it reads the
// current body once, parses it as a JSON object, and installs the result
// via SetRequestBody. A request already carrying a *ReadOnlyJsonBody is
// returned as is. When the body is empty or not a JSON object, the raw
// bytes are re-installed unchanged and the error describes the failure.
func FromBody(r *http.Request) (*ReadOnlyJsonBody, error) {
	if b, ok := r.Body.(*ReadOnlyJsonBody); ok {
		return b, nil
	}
	if r.Body == nil {
		return nil, fmt.Errorf("empty body")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty body")
	}
	b, err := NewJsonBodyFromBytes(raw)
	if err != nil {
		SetRawRequestBody(r, raw) // keep the request readable on the error path
		return nil, err
	}
	SetRequestBody(r, b)
	return b, nil
}

// SetRawRequestBody installs raw as the request's body with length/GetBody
// rewired — the plain-bytes counterpart of SetRequestBody, used to restore
// bodies that failed to parse as JSON objects.
func SetRawRequestBody(r *http.Request, raw []byte) {
	r.Body = io.NopCloser(bytes.NewReader(raw))
	r.ContentLength = int64(len(raw))
	r.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	}
}

// SetRequestBody installs b as the request's body. The wire length is only
// known once the bytes are marshaled, so the request is switched to chunked
// transfer (ContentLength -1) and GetBody serves retries from the cached
// bytes with a fresh reader each call.
func SetRequestBody(r *http.Request, b *ReadOnlyJsonBody) {
	r.Body = b
	r.ContentLength = -1
	r.Header.Del("Content-Length")
	r.GetBody = func() (io.ReadCloser, error) {
		if err := b.marshal(); err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(b.raw)), nil
	}
}
