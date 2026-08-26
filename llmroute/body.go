package llmroute

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Body is an in-memory JSON request body: the parsed object is authoritative.
// Handlers in the chain can type-assert it to read or modify the JSON without
// byte-level reads; it also satisfies io.ReadCloser for regular consumption
// (the bytes are marshaled from the object once, on first Read).
type Body struct {
	// Obj is the authoritative parsed JSON object. It may be read at any
	// time; mutating it is only effective while Readonly() is false, i.e.
	// before the first Read (or GetBody call) has frozen the bytes.
	Obj map[string]any

	// data is the marshal-once byte form: nil until the body is first
	// consumed, non-nil (frozen) afterwards.
	data []byte
	off  int
	eof  bool
}

// FromBytes parses raw as a JSON object.
func FromBytes(raw []byte) (*Body, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if obj == nil { // "null" unmarshals to a nil map without error
		return nil, io.ErrUnexpectedEOF
	}
	return New(obj), nil
}

// New wraps an already-parsed JSON object. The map is owned by the Body from
// here on; callers must not retain or mutate it.
func New(obj map[string]any) *Body { return &Body{Obj: obj} }

// Readonly reports whether the wire bytes are already fixed: once the body
// has been Read (or handed out via GetBody), mutating Obj no longer has any
// effect. Check this before modifying Obj.
func (b *Body) Readonly() bool { return b.data != nil }

// marshal freezes the byte form from Obj.
func (b *Body) marshal() error {
	if b.data != nil {
		return nil
	}
	raw, err := json.Marshal(b.Obj)
	if err != nil {
		return err
	}
	b.data = raw
	return nil
}

// Read streams the marshaled bytes; the first call freezes them.
func (b *Body) Read(p []byte) (int, error) {
	if b.eof {
		return 0, io.EOF
	}
	if b.data == nil {
		if err := b.marshal(); err != nil {
			return 0, err
		}
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	if b.off >= len(b.data) {
		b.eof = true
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// Close is a no-op; the bytes live in memory.
func (b *Body) Close() error { return nil }

var _ io.ReadCloser = (*Body)(nil)

// FromBody converts the request's body into its parsed *Body form: it reads
// the current body once, parses it as a JSON object, and installs the result
// via SetRequestBody. A request already carrying a *Body is returned as is.
// When the body is empty or not a JSON object, the raw bytes are re-installed
// unchanged and the error describes the failure.
func FromBody(r *http.Request) (*Body, error) {
	if b, ok := r.Body.(*Body); ok {
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
	b, err := FromBytes(raw)
	if err != nil {
		resetBody(r, raw) // keep the request readable on the error path
		return nil, err
	}
	SetRequestBody(r, b)
	return b, nil
}

// SetRequestBody installs b as the request's body. The length is only known
// once the bytes are marshaled, so the request is switched to chunked
// transfer (ContentLength -1) and GetBody serves retries from the frozen
// bytes with a fresh reader each call.
func SetRequestBody(r *http.Request, b *Body) {
	r.Body = b
	r.ContentLength = -1
	r.Header.Del("Content-Length")
	r.GetBody = func() (io.ReadCloser, error) {
		if b.data == nil {
			if err := b.marshal(); err != nil {
				return nil, err
			}
		}
		return io.NopCloser(bytes.NewReader(b.data)), nil
	}
}

// deepCopyObj recursively copies a parsed JSON object (maps, slices, and the
// scalar values json.Unmarshal produces) so clones never share state.
func deepCopyObj(obj map[string]any) map[string]any {
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyObj(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	default:
		return v
	}
}
