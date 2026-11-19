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
		{"fixed agent typed", []string{"--agnt", "claude", "codex", "explain"}, []string{"--agent", "claude", "codex", "explain"}, true},
		{"tool", []string{"--tool", "hi"}, []string{"--tool", "hi"}, false},
		{"hosts", []string{"--hosts", "h", "hi"}, []string{"--hosts", "h", "hi"}, false},
		{"envs", []string{"--envs=A=1", "hi"}, []string{"--envs=A=1", "hi"}, false},
		{"allows", []string{"--allows", "Bash", "hi"}, []string{"--allows", "Bash", "hi"}, false},
		{"permissions all", []string{"--permissions", "all"}, []string{"--permissions", "all"}, false},
	}
	for _, c := range cases {
		got, notes, changed := fixSchema().Fix(c.args, roster)
		if !slices.Equal(got, c.want) || changed != c.wantChanged || len(notes) == 0 {
			t.Errorf("%s: Fix(%q) = %q changed=%v notes=%q, want %q changed=%v", c.name, c.args, got, changed, notes, c.want, c.wantChanged)
		}
	}
}

func fixSchema() Schema {
	s := testSchema()
	for _, n := range []string{"tools", "host", "env", "allow"} {
		s.Fields = append(s.Fields, Field{Name: n, Kind: "string"})
	}
	s.Fields = append(s.Fields, Field{Name: "permissions", Kind: "string", Choices: []string{"allow", "plan"}})
	return s
}
