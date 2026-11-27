package statusline

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestPSParentTTY(t *testing.T) {
	t.Run("native", func(t *testing.T) { testPSParentTTYNative(t, psParentTTY) })
	t.Run("invalid pid", func(t *testing.T) { testPSParentTTYInvalidPID(t, psParentTTY) })
	t.Run("context", func(t *testing.T) { testPSParentTTYContext(t, psParentTTY) })
}

func TestDarwinTTYName(t *testing.T) {
	for _, tc := range []struct {
		name         string
		major, minor uint32
		want         string
	}{
		{"first pty", 16, 0, "ttys000"},
		{"padded pty", 16, 7, "ttys007"},
		{"three digit pty", 16, 123, "ttys123"},
		{"large pty", 16, 1234, "ttys1234"},
		{"console", 0, 0, "console"},
		{"controlling tty", 2, 0, "tty"},
		{"legacy first", 4, 0, "ttyp0"},
		{"legacy hex", 4, 31, "ttyqf"},
		{"legacy last", 4, 127, "ttywf"},
		{"legacy out of range", 4, 128, ""},
		{"unknown", 99, 0, ""},
		{"no tty", 255, 0xffffff, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := darwinTTYName(uint32(unix.Mkdev(tc.major, tc.minor))); got != tc.want {
				t.Errorf("name = %q, want %q", got, tc.want)
			}
		})
	}
}
