package launch

import (
	"io"
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// TestMain runs in the hermetic host (hosttest), so no test touches the live vault or sees a vendor CLI.
func TestMain(m *testing.M) {
	NoticeOut = io.Discard
	os.Exit(hosttest.Main(m))
}
