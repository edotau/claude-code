package agent

import (
	"io"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/launch"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	launch.NoticeOut = io.Discard
	os.Exit(hosttest.Main(m))
}
