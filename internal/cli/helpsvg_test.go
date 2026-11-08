package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

// The README's menu image is the current menu: a verb added or reworded without `make help-svg` fails the gate.
func TestMenuSVGIsCurrent(t *testing.T) {
	got, err := os.ReadFile(filepath.Join("..", "..", menuSVGPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, menuSVG()) {
		t.Fatalf("%s is stale: run `make help-svg` and commit it", menuSVGPath)
	}
}

func TestMenuSVGReportsStdoutFailure(t *testing.T) {
	hosttest.ClosedStdout(t)
	if got := cmdMenuSVG(nil); got == 0 {
		t.Fatal("help-svg exit = 0 on closed stdout, want nonzero")
	}
}
