package browser

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestMain(m *testing.M) {
	hosttest.Tools = append(append([]string(nil), hosttest.Tools...), "node")
	os.Exit(hosttest.Main(m))
}
