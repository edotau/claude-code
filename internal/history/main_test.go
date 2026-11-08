package history

import (
	"os"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
)

func TestMain(m *testing.M) {
	code := hosttest.Main(m)
	gittest.Cleanup()
	os.Exit(code)
}
