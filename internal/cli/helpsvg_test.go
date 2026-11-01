package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestConfigureHelpShowsDoubleDashFlags(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	_, _, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"-h"}) })
	if !strings.Contains(stderr, "--restore") {
		t.Errorf("help lacks --restore:\n%s", stderr)
	}
	if strings.Contains(stderr, "\n  -restore") {
		t.Errorf("help prints single-dash -restore:\n%s", stderr)
	}
}

func TestMenuSVGReportsStdoutFailure(t *testing.T) {
	closedStdout(t)
	if got := cmdMenuSVG(nil); got == 0 {
		t.Fatal("help-svg exit = 0 on closed stdout, want nonzero")
	}
}
