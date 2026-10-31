package project

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
