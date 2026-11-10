package mcphealth

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

// closedTestOutput is an already-closed file, so every write to it fails.
func closedTestOutput(t *testing.T) *os.File {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "output")
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}
