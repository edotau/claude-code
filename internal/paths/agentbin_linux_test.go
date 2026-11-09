package paths_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAgentBinRejectsExecutableFIFO(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fifo := filepath.Join(binDirReady(t), "codex")
	if err := syscall.Mkfifo(fifo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fifo, 0o755); err != nil {
		t.Fatal(err)
	}
	codexErrContains(t, "not executable", fifo)
}
