package databricks

import (
	"reflect"
	"testing"
)

// Characterizes maskPair across objects, arrays, headers and one-sided documents.
func TestMaskPair(t *testing.T) {
	o := map[string]any{"api_key": "a", "name": "x", "headers": map[string]any{"X": "h"}, "gone": "g", "list": []any{map[string]any{"token": "t"}}}
	n := map[string]any{"api_key": "b", "name": "y", "headers": map[string]any{"X": "h"}, "added": "z", "clientSecret": "s", "list": []any{map[string]any{"token": "t"}, "extra"}}
	go1, gn := maskPair(o, n, false, "")
	wantO := map[string]any{"api_key": redacted, "name": "x", "headers": map[string]any{"X": redacted}, "gone": "g",
		"list": []any{map[string]any{"token": redacted}}}
	wantN := map[string]any{"api_key": redactedChanged, "name": "y", "headers": map[string]any{"X": redacted}, "added": "z",
		"clientSecret": redacted, "list": []any{map[string]any{"token": redacted}, "extra"}}
	if !reflect.DeepEqual(go1, wantO) || !reflect.DeepEqual(gn, wantN) {
		t.Fatalf("got\n%v\n%v\nwant\n%v\n%v", go1, gn, wantO, wantN)
	}
	// One side is not an object: it comes back untouched while the object side is masked.
	ro, rn := maskPair("plain", map[string]any{"password": "p"}, false, "")
	if ro != "plain" || !reflect.DeepEqual(rn, map[string]any{"password": redacted}) {
		t.Fatalf("one-sided: %v %v", ro, rn)
	}
	// Leaf: a non-credential key stays; a credential key with equal values is not marked changed.
	if a, b := maskPair("v", "w", false, "name"); a != "v" || b != "w" {
		t.Fatalf("plain leaf: %v %v", a, b)
	}
	if a, b := maskPair("v", "v", false, "token"); a != redacted || b != redacted {
		t.Fatalf("equal secret: %v %v", a, b)
	}
}
