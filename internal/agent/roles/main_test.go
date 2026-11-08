package roles

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest/gittest"
)

func TestMain(m *testing.M) {
	code := m.Run()
	gittest.Cleanup()
	os.Exit(code)
}
