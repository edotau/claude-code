package launch

import (
	"slices"
	"testing"
)

// TestEnvWithCustomLosesToSetAndUnset: a custom var overrides the inherited env but never a launch Set or Unset key.
func TestEnvWithCustomLosesToSetAndUnset(t *testing.T) {
	p := Plan{Set: map[string]string{"ANTHROPIC_MODEL": "harness"}, Unset: []string{"CLAUDE_X"}}
	custom := map[string]string{"ANTHROPIC_MODEL": "mine", "CLAUDE_X": "mine", "FOO": "bar", "PATH": "/custom"}
	env := p.envWith([]string{"PATH=/usr/bin", "KEEP=1", "CLAUDE_X=old"}, custom)
	for _, want := range []string{"ANTHROPIC_MODEL=harness", "FOO=bar", "PATH=/custom", "KEEP=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
	for _, bad := range []string{"ANTHROPIC_MODEL=mine", "CLAUDE_X=mine", "CLAUDE_X=old", "PATH=/usr/bin"} {
		if slices.Contains(env, bad) {
			t.Errorf("env has %q: %v", bad, env)
		}
	}
}
