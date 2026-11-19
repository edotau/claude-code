package hooks

import (
	"testing"

	"github.com/edotau/claude-code/internal/plans"
)

func TestUntiedTask(t *testing.T) {
	a := plans.Task{ID: "A", Owns: []string{"w/a.go"}}
	b := plans.Task{ID: "B", Owns: []string{"x/"}}
	done := plans.Task{ID: "D", Owns: []string{"y/d.go"}, Done: true}
	for _, c := range []struct {
		name  string
		dirty []string
		want  string
	}{
		{"one owner", []string{"y/d.go", "z.go", "w/a.go"}, "A"},
		{"two owners", []string{"w/a.go", "x/b.go"}, ""},
		{"no owner", []string{"z.go"}, ""},
		{"done task skipped", []string{"y/d.go"}, ""},
	} {
		got, ok := untiedTask(plans.Plan{Tasks: []plans.Task{a, b, done}}, c.dirty)
		if got.ID != c.want || ok != (c.want != "") {
			t.Errorf("%s: got %q %v, want %q", c.name, got.ID, ok, c.want)
		}
	}
}
