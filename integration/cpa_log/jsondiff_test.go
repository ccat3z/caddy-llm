package cpa_log

import (
	"encoding/json"
	"fmt"
	"strings"
)

// jsonDiff returns a list of "path: want != got" lines describing the
// differences between two decoded JSON values (max ~20 reported).
func jsonDiff(want, got any) []string {
	var diffs []string
	var walk func(path string, a, b any)
	walk = func(path string, a, b any) {
		if len(diffs) > 20 {
			return
		}
		if jsonDeepEq(a, b) {
			return
		}
		av, aok := a.(map[string]any)
		bv, bok := b.(map[string]any)
		if aok && bok {
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
					diffs = append(diffs, fmt.Sprintf("%s: unexpected field = %s", p, trunc(fmt.Sprint(bvv))))
				case !inB:
					diffs = append(diffs, fmt.Sprintf("%s: missing field (want %s)", p, trunc(fmt.Sprint(avv))))
				default:
					walk(p, avv, bvv)
				}
			}
			return
		}
		as, aok := a.([]any)
		bs, bok := b.([]any)
		if aok && bok {
			if len(as) != len(bs) {
				diffs = append(diffs, fmt.Sprintf("%s: length want %d != got %d", path, len(as), len(bs)))
			}
			for i := 0; i < len(as) && i < len(bs); i++ {
				walk(fmt.Sprintf("%s[%d]", path, i), as[i], bs[i])
			}
			return
		}
		diffs = append(diffs, fmt.Sprintf("%s:\n    want: %s\n     got: %s", path, trunc(string(mustMarshal(a))), trunc(string(mustMarshal(b)))))
	}
	walk("$", want, got)
	return diffs
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func trunc(s string) string {
	const max = 600
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// assertJSONDiffEq fails with a field-level diff when want and got differ.
func assertJSONDiffEq(t testingT, what string, wantRaw, gotRaw []byte) {
	var want, got any
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatalf("%s: golden JSON unparseable: %v", what, err)
	}
	if err := json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatalf("%s: translated JSON unparseable: %v", what, err)
	}
	if diffs := jsonDiff(want, got); len(diffs) > 0 {
		t.Errorf("%s mismatch (%d diffs):\n%s", what, len(diffs), strings.Join(diffs, "\n"))
	}
}

type testingT interface {
	Helper()
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
}
