package providers

import (
	"reflect"
	"testing"
)

type diffInner struct {
	Profile string `json:"profile,omitempty"`
}

type diffRec struct {
	Model  string            `json:"model,omitempty"`
	Route  diffInner         `json:"route,omitzero"`
	Alias  map[string]string `json:"aliases,omitempty"`
	List   []string          `json:"list,omitempty"`
	Hidden string            `json:"-"`
}

func TestStructDiff(t *testing.T) {
	a := diffRec{Model: "m", Route: diffInner{"a"}, Alias: map[string]string{"opus": "x", "gone": "y"}, Hidden: "1"}
	b := diffRec{Route: diffInner{"b"}, Alias: map[string]string{"opus": "x", "new": "z"}, List: []string{"q"}, Hidden: "2"}
	want := []structChange{
		{"aliases.gone", "y", nil}, {"aliases.new", nil, "z"}, {"list", nil, []string{"q"}},
		{"model", "m", nil}, {"route.profile", "a", "b"},
	}
	if got := diffStructs(a, b); !reflect.DeepEqual(got, want) {
		t.Fatalf("Diff = %v, want %v", got, want)
	}
	if got := diffStructs(diffRec{}, diffRec{Alias: map[string]string{}}); got != nil {
		t.Fatalf("zero vs empty map = %v, want none", got)
	}
	if got := diffLine(want[1], nil); got != "aliases.new: (unset) → z" {
		t.Fatalf("Line = %q", got)
	}
	if got := diffLine(want[1], func(string) bool { return true }); got != "aliases.new: (changed)" {
		t.Fatalf("redacted Line = %q", got)
	}
}
