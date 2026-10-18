package cli

import (
	"bytes"
	"os"
	"path/filepath"
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
