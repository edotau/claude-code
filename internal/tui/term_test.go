package tui

import (
	"os"
	"testing"
)

// Open requires a real TTY on stdin; under `go test` stdin is never one, so this only proves the
// clean failure path. A raw-mode exercise of Open/Close runs manually via a pty, not in CI.
func TestOpenFailsCleanlyWithoutATTY(t *testing.T) {
	if isTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a TTY in this environment; the no-TTY path isn't exercised here")
	}
	tm, err := Open()
	if err == nil {
		tm.Close()
		t.Fatal("Open() succeeded without a TTY on stdin")
	}
}
