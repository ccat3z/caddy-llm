package caddy_llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustNode(t *testing.T, raw string) *LazyJsonNode {
	t.Helper()
	return &LazyJsonNode{Val: json.RawMessage(raw)}
}

func TestTypeWithoutPromotion(t *testing.T) {
	n := mustNode(t, `{"a":1}`)
	if n.Type() != TypeObject {
		t.Errorf("Type = %v", n.Type())
	}
	if _, isRaw := n.Val.(json.RawMessage); !isRaw {
		t.Error("Type must not promote the form")
	}
	for raw, want := range map[string]Type{
		`[1,2]`:   TypeArray,
		`"x"`:     TypeString,
		`42`:      TypeNumber,
		`4.5`:     TypeNumber,
		`true`:    TypeBool,
		`false`:   TypeBool,
		`null`:    TypeNull,
		`{"a":1}`: TypeObject,
	} {
		if got := mustNode(t, raw).Type(); got != want {
			t.Errorf("Type(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestScalars(t *testing.T) {
	if s, ok := mustNode(t, `"hi"`).String(); !ok || s != "hi" {
		t.Errorf("String = %q %v", s, ok)
	}
	if b, ok := mustNode(t, `true`).Bool(); !ok || !b {
		t.Errorf("Bool = %v %v", b, ok)
	}
	if f, ok := mustNode(t, `4.25`).Float64(); !ok || f != 4.25 {
		t.Errorf("Float64 = %v %v", f, ok)
	}
	if i, ok := mustNode(t, `42`).Int(); !ok || i != 42 {
		t.Errorf("Int = %v %v", i, ok)
	}
	// Int rejects non-integral numbers.
	if _, ok := mustNode(t, `4.5`).Int(); ok {
		t.Error("Int(4.5) should fail")
	}
	// Wrong-type accessors report absence.
	if _, ok := mustNode(t, `42`).String(); ok {
		t.Error("String(42) should fail")
	}
	if _, ok := mustNode(t, `"x"`).Bool(); ok {
		t.Error("Bool(string) should fail")
	}
}

func TestNumberPreservesIntegerForm(t *testing.T) {
	n := mustNode(t, `2000000`)
	if _, ok := n.Float64(); !ok {
		t.Fatal("Float64 failed")
	}
	// After resolution the wire form keeps the integer literal (no 2e+06).
	out, err := n.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `2000000` {
		t.Errorf("Marshal after Float64 = %s, want 2000000", out)
	}
}

func TestGetAndObj(t *testing.T) {
	n := mustNode(t, `{"model":"glm-5.2","nested":{"deep":[1,{"x":2}]},"nullv":null}`)

	m, ok := n.Get("model").String()
	if !ok || m != "glm-5.2" {
		t.Errorf("model = %q %v", m, ok)
	}

	// Nested laziness: the nested sub-node is still raw until accessed.
	deep := n.Get("nested")
	if _, isRaw := deep.Val.(json.RawMessage); !isRaw {
		t.Error("sub-node must stay raw before access")
	}
	x, ok := deep.Get("deep").List()
	if !ok || len(x) != 2 {
		t.Fatalf("deep list = %v %v", x, ok)
	}
	xv, ok := x[1].Get("x").Int()
	if !ok || xv != 2 {
		t.Errorf("x = %v %v", xv, ok)
	}

	// Explicit null member: exists, Type null.
	nullNode := n.Get("nullv")
	if nullNode.Type() != TypeNull {
		t.Errorf("null member type = %v", nullNode.Type())
	}

	// Obj: member table of lazy sub-nodes.
	obj, ok := n.Obj()
	if !ok || len(obj) != 3 {
		t.Fatalf("Obj = %v %v", len(obj), ok)
	}
	if sm, ok := obj["model"].String(); !ok || sm != "glm-5.2" {
		t.Errorf("obj[model] = %q", sm)
	}

	// Missing key → NotExist.
	if nt := n.Get("nope"); nt.Type() != TypeNotExist {
		t.Errorf("missing type = %v", nt.Type())
	}
	// Get on non-object → NotExist.
	if nt := mustNode(t, `[1]`).Get("a"); nt.Type() != TypeNotExist {
		t.Errorf("non-object Get type = %v", nt.Type())
	}
}

func TestListAndLen(t *testing.T) {
	n := mustNode(t, `[{"a":1},{"a":2},3]`)
	items, ok := n.List()
	if !ok || len(items) != 3 {
		t.Fatalf("List = %d %v", len(items), ok)
	}
	if a, ok := items[1].Get("a").Int(); !ok || a != 2 {
		t.Errorf("items[1].a = %v", a)
	}
	if l, ok := n.Len(); !ok || l != 3 {
		t.Errorf("Len = %v %v", l, ok)
	}
	if _, ok := mustNode(t, `{"a":1}`).List(); ok {
		t.Error("List(object) should fail")
	}
	if l, ok := mustNode(t, `{"a":1,"b":2}`).Len(); !ok || l != 2 {
		t.Errorf("object Len = %v", l)
	}
}

func TestWithTopLevel(t *testing.T) {
	n := mustNode(t, `{"model":"mc/x","keep":{"untouched":[1,2]}}`)
	w := n.With("model", "glm-5.2")

	// Original untouched.
	if m, _ := n.Get("model").String(); m != "mc/x" {
		t.Errorf("original mutated: %q", m)
	}
	if m, _ := w.Get("model").String(); m != "glm-5.2" {
		t.Errorf("variant model = %q", m)
	}
	// Siblings survive.
	k, ok := w.Get("keep").Get("untouched").List()
	if !ok || len(k) != 2 {
		t.Errorf("sibling lost: %v", k)
	}
}

func TestWithDeepPath(t *testing.T) {
	n := mustNode(t, `{"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}],"other":true}`)
	w := n.With("messages.1.content", "CHANGED")

	origItems, _ := n.Get("messages").List()
	if m, _ := origItems[1].Get("content").String(); m != "b" {
		t.Errorf("original mutated: %q", m)
	}
	items, _ := w.Get("messages").List()
	if m, _ := items[0].Get("content").String(); m != "a" {
		t.Errorf("sibling element mutated: %q", m)
	}
	if m, _ := items[1].Get("content").String(); m != "CHANGED" {
		t.Errorf("target not replaced: %q", m)
	}
	if b, _ := w.Get("other").Bool(); !b {
		t.Error("sibling key lost")
	}
}

func TestWithChained(t *testing.T) {
	n := mustNode(t, `{"a":1,"b":2}`)
	w := n.With("a", 10).With("b", 20)
	if a, _ := w.Get("a").Int(); a != 10 {
		t.Errorf("a = %v", a)
	}
	if b, _ := w.Get("b").Int(); b != 20 {
		t.Errorf("b = %v", b)
	}
}

func TestMarshalForms(t *testing.T) {
	// Raw form: zero cost, original bytes.
	raw := json.RawMessage(`  {"a": 1, "b":[2,3]}  `)
	n := &LazyJsonNode{Val: raw}
	out, err := n.Marshal()
	if err != nil || &out[0] != &raw[0] { //nolint:staticcheck // aliasing is the point
		t.Errorf("raw Marshal should return the original bytes")
	}

	// Segmented form: encodes (key order may change; content preserved).
	seg := mustNode(t, `{"a":1,"b":[2,3]}`)
	seg.Get("a").Int() // promote
	out, err = seg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	json.Unmarshal(out, &got)
	json.Unmarshal([]byte(`{"a":1,"b":[2,3]}`), &want)
	if js := mustJSON(got); js != mustJSON(want) {
		t.Errorf("segmented Marshal = %s", out)
	}
	_ = strings.TrimSpace
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestMarshalGoValue(t *testing.T) {
	n := &LazyJsonNode{Val: map[string]any{"x": "y"}}
	out, err := n.Marshal()
	if err != nil || string(out) != `{"x":"y"}` {
		t.Errorf("Go value Marshal = %s %v", out, err)
	}
}
