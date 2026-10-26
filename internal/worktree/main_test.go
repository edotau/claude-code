package worktree

import (
	"fmt"
	"os"
	"testing"
)

// TestMain pins every state/config path to a temp dir so no test can touch the real ~/.claude, and trims git startup.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "worktree-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for k, v := range map[string]string{
		"HOME": tmp, "CLAUDE_CONFIG_DIR": tmp, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0",
	} {
		os.Setenv(k, v)
	}
	code := m.Run()
	os.RemoveAll(tmp)
	if tmplDir != "" {
		os.RemoveAll(tmplDir)
	}
	os.Exit(code)
}
