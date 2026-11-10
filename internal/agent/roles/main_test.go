package roles

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	hosttest.Cleanup()
	os.Exit(code)
}
