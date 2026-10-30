package copilotsdk

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// TestMain runs in the hermetic host (hosttest), so no test touches the live config or vault.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
