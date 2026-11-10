package helptext

import (
	"errors"
	"io"
	"os"
	"testing"
)

func captureStderr(t *testing.T, f func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	code := f()
	os.Stderr = old
	w.Close()
	b, _ := io.ReadAll(r)
	return string(b), code
}

// BadFormat must print the prefixed one-line error and exit 2; Fail exits 1.
func TestFailExitCodes(t *testing.T) {
	out, code := captureStderr(t, func() int { return BadFormat("vault ls", errors.New("bad")) })
	if out != "claude-code: vault ls: bad\n" || code != 2 {
		t.Errorf("BadFormat = %q, %d", out, code)
	}
	out, code = captureStderr(t, func() int { return Fail("x %d", 1) })
	if out != "claude-code: x 1\n" || code != 1 {
		t.Errorf("Fail = %q, %d", out, code)
	}
}
