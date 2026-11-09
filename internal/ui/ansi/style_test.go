package ansi

import (
	"os"
	"runtime"
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

func TestIsTerminalRejectsDevZero(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("termios terminal detection is supported on Linux and Darwin")
	}
	f, err := os.Open("/dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	if IsTerminal(f) {
		t.Error("IsTerminal(/dev/zero) = true, want false")
	}
}

// TestStatusNotVerifiedStaysGray: the longer "not verified" token wins, so its "verified" is never painted green.
func TestStatusNotVerifiedStaysGray(t *testing.T) {
	got := Status(true, "gemini  not verified")
	if want := "gemini  " + Dim + "not verified" + Reset; got != want {
		t.Fatalf("Status = %q, want %q", got, want)
	}
	if got := Status(true, "verified"); got != Green+"verified"+Reset {
		t.Fatalf("Status(verified) = %q", got)
	}
}
