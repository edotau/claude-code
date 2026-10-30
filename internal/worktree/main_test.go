package worktree

import (
	"github.com/edotau/claude-code/internal/hosttest"

	"os"
	"testing"
)

// TestMain pins every state/config path to a temp dir so no test can touch the real ~/.claude, and trims git startup.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	for k, v := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0",
	} {
		os.Setenv(k, v)
	}
	code := m.Run()
	cleanup()
	if tmplDir != "" {
		os.RemoveAll(tmplDir)
	}
	os.Exit(code)
}
