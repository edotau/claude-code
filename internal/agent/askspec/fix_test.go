package askspec

import (
	"slices"
	"testing"
)

func TestFix(t *testing.T) {
	roster := []string{"copilot", "codex"}
	cases := []struct {
		name        string
		args, want  []string
		wantChanged bool
	}{
		{"flag typo", []string{"--modle", "opus", "hi"}, []string{"--model", "opus", "hi"}, true},
		{"agent word", []string{"copilot", "hi"}, []string{"--agent", "copilot", "hi"}, true},
		{"task-file", []string{"--task-file", "-"}, []string{"-"}, true},
		{"choice", []string{"--fallback", "agent"}, []string{"--fallback", "agents"}, true},
		{"no near flag", []string{"--zzzzzz", "x"}, []string{"--zzzzzz", "x"}, false},
	}
	for _, c := range cases {
		got, notes, changed := testSchema().Fix(c.args, roster)
		if !slices.Equal(got, c.want) || changed != c.wantChanged || len(notes) == 0 {
			t.Errorf("%s: Fix(%q) = %q changed=%v notes=%q, want %q changed=%v", c.name, c.args, got, changed, notes, c.want, c.wantChanged)
		}
	}
}
