package llmroute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	caddyllm "github.com/ccat3z/caddy-llm"
)

// ReadOnlyJsonBody is an immutable, lazily-parsed JSON request body. The
// embedded caddyllm.LazyJsonNode gives on-demand access (Get/Obj/List/With)
// with per-node lazy parsing; this wrapper adds the HTTP/io side. The raw
// cache keeps the wire bytes stable once materialized, so the Read cursor
// (and GetBody retries) always serve the same bytes.
//
// To "modify" a body, build a new one (With on the node returns a new node)
// and install it with SetRequestBody. Bodies are never mutated in place.
type ReadOnlyJsonBody struct {
	caddyllm.LazyJsonNode // embedded; field name is LazyJsonNode
	// raw caches the materialized wire bytes: set at construction when the
	// body came from bytes, or produced once by Marshal. Keeps Read's
	// cursor consistent across repeated Marshal/Read/GetBody calls.
	raw []byte
	off int
	eof bool
}

// NewJsonBody wraps raw bytes: construction is free (no parsing), the first
// accessor decides what gets parsed.
func NewJsonBody(raw []byte) *ReadOnlyJsonBody {
	return &ReadOnlyJsonBody{LazyJsonNode: caddyllm.LazyJsonNode{Val: json.RawMessage(raw)}}
}

// Marshal returns the body's wire bytes, encoding them once from the node's
// current form if the body was built from values. The result is cached, so
// repeated calls (and Read/GetBody) serve identical bytes.
func (b *ReadOnlyJsonBody) Marshal() ([]byte, error) {
	if b.raw == nil {
		raw, err := b.LazyJsonNode.Marshal()
		if err != nil {
			return nil, err
		}
		b.raw = raw
	}
	return b.raw, nil
}

// Read streams the wire bytes (materializing them on first use).
func (b *ReadOnlyJsonBody) Read(p []byte) (int, error) {
	if b.eof {
		return 0, io.EOF
	}
	if b.raw == nil {
		if err := b.materialize(); err != nil {
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

func (b *ReadOnlyJsonBody) materialize() error {
	raw, err := b.LazyJsonNode.Marshal()
	if err != nil {
		return err
	}
	b.raw = raw
	return nil
}

// Close is a no-op; the bytes live in memory.
func (b *ReadOnlyJsonBody) Close() error { return nil }

var _ io.ReadCloser = (*ReadOnlyJsonBody)(nil)

// NewJsonBodyFromBytes parses raw as a JSON object.
func NewJsonBodyFromBytes(raw []byte) (*ReadOnlyJsonBody, error) {
	b := NewJsonBody(raw)
	// Validate: the router and translator both assume a top-level object.
	if t := b.Type(); t != caddyllm.TypeObject {
		if t == caddyllm.TypeNull {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("request body is not a JSON object")
	}
	return b, nil
}

// FromBody converts the request's body into its parsed form: it reads the
// current body once and installs a *ReadOnlyJsonBody over the bytes. A
// request already carrying one is returned as is. When the body is empty or
// not a JSON object, the raw bytes are re-installed unchanged and the error
// describes the failure.
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
		if err := b.materialize(); err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(b.raw)), nil
	}
}
