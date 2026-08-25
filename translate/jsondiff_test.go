package translate

import (
	"fmt"
	"strings"
)

// jsonSemanticsDiff returns "path: a != b" lines for semantic differences
// between two decoded JSON values (nil/empty treated equal, float64
// compared exactly). Same comparison rules as the cpa regression diff.
func jsonSemanticsDiff(path string, a, b any) []string {
	if jsonEq(a, b) {
		return nil
	}
	av, aok := a.(map[string]any)
	bv, bok := b.(map[string]any)
	if aok && bok {
		var diffs []string
		keys := map[string]struct{}{}
		for k := range av {
			keys[k] = struct{}{}
		}
		for k := range bv {
			keys[k] = struct{}{}
		}
		for k := range keys {
			p := path + "." + k
			avv, inA := av[k]
			bvv, inB := bv[k]
			switch {
			case !inA:
				if !bothNilOrEmptyJSON(nil, bvv) { // absent vs null/empty: wire-equal
					diffs = append(diffs, fmt.Sprintf("%s: unexpected field = %v", p, truncStr(fmt.Sprint(bvv))))
				}
			case !inB:
				if !bothNilOrEmptyJSON(avv, nil) { // null/empty vs absent: wire-equal
					diffs = append(diffs, fmt.Sprintf("%s: missing field (want %v)", p, truncStr(fmt.Sprint(avv))))
				}
			default:
				diffs = append(diffs, jsonSemanticsDiff(p, avv, bvv)...)
			}
		}
		return diffs
	}
	as, asOk := a.([]any)
	bs, bsOk := b.([]any)
	if asOk && bsOk {
		var diffs []string
		if len(as) != len(bs) {
			return []string{fmt.Sprintf("%s: array length %d != %d", path, len(as), len(bs))}
		}
		for i := range as {
			diffs = append(diffs, jsonSemanticsDiff(fmt.Sprintf("%s[%d]", path, i), as[i], bs[i])...)
		}
		return diffs
	}
	return []string{fmt.Sprintf("%s: %v != %v", path, a, b)}
}

// jsonEq is a semantic equality check for decoded JSON values.
// null vs absent are treated equal (wire-equivalent), matching the cpa
// regression comparison rules.
func jsonEq(a, b any) bool {
	if a == nil || b == nil {
		return bothNilOrEmptyJSON(a, b)
	}
	switch av := a.(type) {
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bvv, in := bv[k]
			if !in || !jsonEq(v, bvv) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEq(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// bothNilOrEmptyJSON reports whether two decoded values are wire-equal
// when one of them is null: null, empty array, and empty map all encode
// equivalently-enough for the regression comparison.
func bothNilOrEmptyJSON(a, b any) bool {
	if a != nil && b != nil {
		return false
	}
	empty := func(v any) bool {
		switch t := v.(type) {
		case nil:
			return true
		case []any:
			return len(t) == 0
		case map[string]any:
			return len(t) == 0
		default:
			return false
		}
	}
	return empty(a) && empty(b)
}

func truncStr(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return strings.TrimSpace(s)
}
