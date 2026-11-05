package quality

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
)

// TestMain isolates the host (HOME, config dir, PATH tools) so no quality test reads or writes the real machine.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	code := m.Run()
	gittest.Cleanup()
	cleanup()
	os.Exit(code)
}
