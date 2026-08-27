// Package internal hosts the LazyJsonNode lazy-JSON value type and the
// JsonReqBody HTTP request body built on it.
package internal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// LazyJsonNode is a lazily-parsed JSON value of any type. Construction is
// free: the node holds its raw bytes. Each accessor promotes the node to
// just the shape it needs (first use parses once and caches; parsing never
// goes backwards). Modifications return a new node — nodes are treated as
// immutable once shared.
//
// The parsing state is carried by Val's concrete type:
//
//	json.RawMessage             → unparsed (construction / sub-node)
//	map[string]json.RawMessage  → object segmented, nested still bytes (first Obj/Get/Keys/Len)
//	[]json.RawMessage           → array segmented (first List/Len)
//	string / json.Number / bool / nil → scalar resolved (first String/Bool/...)
//
// Any other Go value is allowed when the node is constructed from one; it
// marshals as-is.
type LazyJsonNode struct {
	Val any
}

// Type is the JSON type of a node.
type Type int

const (
	TypeInvalid Type = iota
	TypeObject
	TypeArray
	TypeString
	TypeNumber
	TypeBool
	TypeNull
	TypeNotExist
)

// notExistVal is the sentinel Val marking an absent node.
type notExistVal struct{}

// notExist is the shared node returned for absent members.
var notExist = &LazyJsonNode{Val: notExistVal{}}

// NotExist returns the shared absent-node (Get on a non-object, missing
// keys). Its Type is TypeNotExist and every accessor reports absence.
func NotExist() *LazyJsonNode { return notExist }

// Type reports the node's JSON type without promoting its form: a raw node
// is classified by its first non-space byte only.
func (n *LazyJsonNode) Type() Type {
	if n == nil {
		return TypeNotExist
	}
	switch v := n.Val.(type) {
	case json.RawMessage:
		switch firstByte(v) {
		case '{':
			return TypeObject
		case '[':
			return TypeArray
		case '"':
			return TypeString
		case 't', 'f':
			return TypeBool
		case 'n':
			return TypeNull
		default:
			return TypeNumber
		}
	case map[string]json.RawMessage:
		return TypeObject
	case []json.RawMessage:
		return TypeArray
	case string:
		return TypeString
	case json.Number:
		return TypeNumber
	case float64, int, int64:
		return TypeNumber
	case bool:
		return TypeBool
	case nil:
		return TypeNull
	case notExistVal:
		return TypeNotExist
	default:
		return TypeObject // constructed from a Go map
	}
}

func firstByte(raw json.RawMessage) byte {
	for _, b := range raw {
		if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
			return b
		}
	}
	return 0
}

// ---------- promotion ----------

// promoteObject segments a raw object one level: each member value stays
// raw bytes (nested content unparsed). Idempotent; returns nil when the
// node is not an object.
func (n *LazyJsonNode) promoteObject() (*LazyJsonNode, map[string]json.RawMessage, bool) {
	switch v := n.Val.(type) {
	case map[string]json.RawMessage:
		return n, v, true
	case json.RawMessage:
		if firstByte(v) != '{' {
			return nil, nil, false
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(v, &m); err != nil {
			return nil, nil, false
		}
		if m == nil { // "null" object literal
			return nil, nil, false
		}
		n.Val = m
		return n, m, true
	case map[string]any:
		rm := make(map[string]json.RawMessage, len(v))
		for k, e := range v {
			rm[k] = mustMarshalValue(e)
		}
		n.Val = rm
		return n, rm, true
	default:
		return nil, nil, false
	}
}

// promoteArray segments a raw array one level.
func (n *LazyJsonNode) promoteArray() (*LazyJsonNode, []json.RawMessage, bool) {
	switch v := n.Val.(type) {
	case []json.RawMessage:
		return n, v, true
	case json.RawMessage:
		if firstByte(v) != '[' {
			return nil, nil, false
		}
		var a []json.RawMessage
		if err := json.Unmarshal(v, &a); err != nil {
			return nil, nil, false
		}
		n.Val = a
		return n, a, true
	case []any:
		rm := make([]json.RawMessage, len(v))
		for i, e := range v {
			rm[i] = mustMarshalValue(e)
		}
		n.Val = rm
		return n, rm, true
	default:
		return nil, nil, false
	}
}

func mustMarshalValue(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}

// ---------- scalar accessors (promote to scalar) ----------

// String resolves the node to a string.
func (n *LazyJsonNode) String() (string, bool) {
	if n == nil {
		return "", false
	}
	if s, ok := n.Val.(string); ok {
		return s, true
	}
	raw, ok := n.Val.(json.RawMessage)
	if !ok || firstByte(raw) != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	n.Val = s
	return s, true
}

// Bool resolves the node to a bool.
func (n *LazyJsonNode) Bool() (bool, bool) {
	if n == nil {
		return false, false
	}
	if b, ok := n.Val.(bool); ok {
		return b, true
	}
	raw, ok := n.Val.(json.RawMessage)
	if !ok {
		return false, false
	}
	fb := firstByte(raw)
	if fb != 't' && fb != 'f' {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, false
	}
	n.Val = b
	return b, true
}

// Float64 resolves the node to a number.
func (n *LazyJsonNode) Float64() (float64, bool) {
	if n == nil {
		return 0, false
	}
	switch v := n.Val.(type) {
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	raw, ok := n.Val.(json.RawMessage)
	if !ok {
		return 0, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var num json.Number
	if err := dec.Decode(&num); err != nil {
		return 0, false
	}
	if _, err := num.Float64(); err != nil {
		return 0, false
	}
	n.Val = num
	f, _ := num.Float64()
	return f, true
}

// Int resolves the node to an integer (the number must be integral).
func (n *LazyJsonNode) Int() (int, bool) {
	if n == nil {
		return 0, false
	}
	switch v := n.Val.(type) {
	case json.Number:
		i, err := strconv.ParseInt(v.String(), 10, 64)
		return int(i), err == nil
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	f, ok := n.Float64()
	if !ok || f != float64(int64(f)) {
		return 0, false
	}
	return int(f), true
}

// ---------- structural accessors ----------

// Get returns the member node for key (a new lazy node over the member's
// raw bytes), or NotExist when this node is not an object or lacks the key.
// A raw node is promoted (one-level segmentation) on first use.
func (n *LazyJsonNode) Get(key string) *LazyJsonNode {
	if n == nil {
		return notExist
	}
	if _, m, ok := n.promoteObject(); ok {
		if rm, ok := m[key]; ok {
			if rm == nil { // explicit JSON null
				return &LazyJsonNode{Val: nil}
			}
			return &LazyJsonNode{Val: rm}
		}
	}
	return notExist
}

// Obj returns the member table of an object: each value is a fresh lazy
// node (still unparsed). Non-objects report false.
func (n *LazyJsonNode) Obj() (map[string]*LazyJsonNode, bool) {
	if n == nil {
		return nil, false
	}
	if _, m, ok := n.promoteObject(); ok {
		out := make(map[string]*LazyJsonNode, len(m))
		for k, rm := range m {
			if rm == nil {
				out[k] = &LazyJsonNode{Val: nil}
				continue
			}
			out[k] = &LazyJsonNode{Val: rm}
		}
		return out, true
	}
	return nil, false
}

// List returns the element nodes of an array (each still lazy).
// Non-arrays report false.
func (n *LazyJsonNode) List() ([]*LazyJsonNode, bool) {
	if n == nil {
		return nil, false
	}
	if _, a, ok := n.promoteArray(); ok {
		out := make([]*LazyJsonNode, len(a))
		for i, rm := range a {
			if rm == nil {
				out[i] = &LazyJsonNode{Val: nil}
				continue
			}
			out[i] = &LazyJsonNode{Val: rm}
		}
		return out, true
	}
	return nil, false
}

// Len returns the member count of an object or the element count of an
// array.
func (n *LazyJsonNode) Len() (int, bool) {
	if n == nil {
		return 0, false
	}
	if _, m, ok := n.promoteObject(); ok {
		return len(m), true
	}
	if _, a, ok := n.promoteArray(); ok {
		return len(a), true
	}
	return 0, false
}

// Keys returns an object's member names.
func (n *LazyJsonNode) Keys() ([]string, bool) {
	if n == nil {
		return nil, false
	}
	if _, m, ok := n.promoteObject(); ok {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		return out, true
	}
	return nil, false
}

// ---------- immutable modification ----------

// With returns a new node with the value at path replaced, e.g.
// "messages.0.content" descends objects by key and arrays by index. Every
// level on the path is shallow-copied (siblings shared, treated as
// read-only); the original node is untouched.
func (n *LazyJsonNode) With(path string, v any) *LazyJsonNode {
	parts := splitPath(path)
	if len(parts) == 0 {
		return n
	}
	return n.withParts(parts, v)
}

func splitPath(path string) []string {
	var parts []string
	for _, seg := range strings.Split(path, ".") {
		if seg != "" {
			parts = append(parts, seg)
		}
	}
	return parts
}

func (n *LazyJsonNode) withParts(parts []string, v any) *LazyJsonNode {
	head := parts[0]
	if len(parts) == 1 {
		return n.withChild(head, v)
	}
	// Interior level: copy this container, descend. Objects descend by key,
	// arrays by numeric index.
	child := n.childAt(head)
	replaced := child.withParts(parts[1:], v)
	return n.withNode(head, replaced)
}

// childAt returns the member (object key or array index) for descending.
func (n *LazyJsonNode) childAt(head string) *LazyJsonNode {
	if _, _, ok := n.promoteObject(); ok {
		return n.Get(head)
	}
	if _, a, ok := n.promoteArray(); ok {
		idx, err := strconv.Atoi(head)
		if err == nil && idx >= 0 && idx < len(a) {
			return &LazyJsonNode{Val: a[idx]}
		}
	}
	return notExist
}

// withChild returns a copy of this object/array with member head replaced
// by the Go value v.
func (n *LazyJsonNode) withChild(head string, v any) *LazyJsonNode {
	return n.withNode(head, &LazyJsonNode{Val: v})
}

// withNode returns a copy of this container with member head replaced by
// node (nil node removes the member).
func (n *LazyJsonNode) withNode(head string, node *LazyJsonNode) *LazyJsonNode {
	if _, m, ok := n.promoteObject(); ok {
		out := make(map[string]json.RawMessage, len(m)+1)
		for k, e := range m {
			out[k] = e
		}
		if node == nil {
			delete(out, head)
		} else {
			out[head] = mustMarshalValue(node.Val)
		}
		return &LazyJsonNode{Val: out}
	}
	if _, a, ok := n.promoteArray(); ok {
		idx, err := strconv.Atoi(head)
		if err != nil || idx < 0 || idx >= len(a) {
			return n // index out of range: unchanged
		}
		out := make([]json.RawMessage, len(a))
		copy(out, a)
		if node == nil {
			out[idx] = json.RawMessage("null")
		} else {
			out[idx] = mustMarshalValue(node.Val)
		}
		return &LazyJsonNode{Val: out}
	}
	return n // not a container: unchanged
}

// ---------- materialization ----------

// Marshal encodes the node per its current form: a raw node's original
// bytes return as-is (zero cost); segmented and Go-value forms encode with
// json.Marshal, which inlines any RawMessage members verbatim. Purely
// functional — the node does not cache the result.
func (n *LazyJsonNode) Marshal() ([]byte, error) {
	if n == nil {
		return []byte("null"), nil
	}
	switch v := n.Val.(type) {
	case json.RawMessage:
		return v, nil
	case notExistVal:
		return nil, fmt.Errorf("marshal of not-exist node")
	default:
		return json.Marshal(v)
	}
}
