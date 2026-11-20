package structdiff

import (
	"reflect"
	"testing"
)

type inner struct {
	Profile string `json:"profile,omitempty"`
}

type rec struct {
	Model  string            `json:"model,omitempty"`
	Route  inner             `json:"route,omitzero"`
	Alias  map[string]string `json:"aliases,omitempty"`
	List   []string          `json:"list,omitempty"`
	Hidden string            `json:"-"`
}

func TestDiff(t *testing.T) {
	a := rec{Model: "m", Route: inner{"a"}, Alias: map[string]string{"opus": "x", "gone": "y"}, Hidden: "1"}
	b := rec{Route: inner{"b"}, Alias: map[string]string{"opus": "x", "new": "z"}, List: []string{"q"}, Hidden: "2"}
	want := []Change{
		{"aliases.gone", "y", nil}, {"aliases.new", nil, "z"}, {"list", nil, []string{"q"}},
		{"model", "m", nil}, {"route.profile", "a", "b"},
	}
	if got := Diff(a, b); !reflect.DeepEqual(got, want) {
		t.Fatalf("Diff = %v, want %v", got, want)
	}
	if got := Diff(rec{}, rec{Alias: map[string]string{}}); got != nil {
		t.Fatalf("zero vs empty map = %v, want none", got)
	}
	if got := Line(want[1], nil); got != "aliases.new: (unset) → z" {
		t.Fatalf("Line = %q", got)
	}
	if got := Line(want[1], func(string) bool { return true }); got != "aliases.new: (changed)" {
		t.Fatalf("redacted Line = %q", got)
	}
}
