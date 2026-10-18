package ansi

import (
	"os"
	"testing"
)

// TestIsTerminalRejectsDevNullAndFiles: /dev/null is a char device but never a terminal.
func TestIsTerminalRejectsDevNullAndFiles(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if IsTerminal(null) {
		t.Error("IsTerminal(/dev/null) = true, want false")
	}
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsTerminal(f) {
		t.Error("IsTerminal(regular file) = true, want false")
	}
}
