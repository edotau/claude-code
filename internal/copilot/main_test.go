package copilot

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// TestMain runs in the hermetic host (hosttest), so no test touches the live config or vault.
func TestMain(m *testing.M) {
	code := hosttest.Main(m)
	removeFakeCLI() // the fake CLI is built once per package (sdk_test.go), so it outlives every test
	os.Exit(code)
}
