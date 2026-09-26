package launch

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestScienceStripsHarnessEnvOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude-science"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	env := []string{"ANTHROPIC_BASE_URL=http://127.0.0.1:4000/p/x", "ANTHROPIC_CUSTOM_HEADERS=x-harness: s",
		"CLAUDE_CODE_SSE_PORT=1", "CLAUDECODE=1", "HARNESS_PROVIDER=p", "GEMINI_CLI_IDE_AUTH_TOKEN=t",
		"CLAUDE_SCIENCE_TOKEN=keep", "ANTHROPIC_ORGANIZATION_ID=keep", "HOME=/h"}
	p, err := Science(env, []string{"serve", "--model", "x"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_SSE_PORT", "CLAUDECODE",
		"HARNESS_PROVIDER", "GEMINI_CLI_IDE_AUTH_TOKEN"}
	if !slices.Equal(p.Unset, want) || !slices.Equal(p.Argv, []string{"serve", "--model", "x"}) {
		t.Errorf("unset %q argv %q, want unset %q and argv passed through", p.Unset, p.Argv, want)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := Science(env, nil); err == nil {
		t.Error("a missing claude-science must error, not exec")
	}
}
