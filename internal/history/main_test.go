package history

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestMain(m *testing.M) {
	code := hosttest.Main(m)
	hosttest.Cleanup()
	os.Exit(code)
}
