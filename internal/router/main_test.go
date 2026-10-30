package router

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// TestMain runs in the hermetic host (hosttest): a test must never read the developer's vault pins.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
