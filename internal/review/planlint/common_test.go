package planlint

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReadInputStdinReadError(t *testing.T) {
	var stderr strings.Builder
	r := iotest.ErrReader(errors.New("broken pipe"))
	text, source, exit, ok := readInput("-", r, &stderr)
	if ok || exit != 1 || text != "" || source != "" {
		t.Fatalf("got (%q, %q, %v), want empty and ok=false", text, source, ok)
	}
	if !strings.Contains(stderr.String(), "broken pipe") {
		t.Fatalf("stderr %q does not name the read error", stderr.String())
	}
}

func TestReadInputStdinOK(t *testing.T) {
	text, source, _, ok := readInput("-", strings.NewReader("hello"), &strings.Builder{})
	if !ok || text != "hello" || source != "stdin" {
		t.Fatalf("got (%q, %q, %v)", text, source, ok)
	}
}

// An unreadable or missing input must exit 1, never pass as clean.
func TestReadInputUnreadableExits1(t *testing.T) {
	_, _, exit, ok := readInput(t.TempDir(), strings.NewReader(""), &strings.Builder{})
	if ok || exit != 1 {
		t.Fatalf("directory input: exit %d ok %v, want 1 false", exit, ok)
	}
	if _, _, exit, ok = readInput("/nonexistent/plan.md", strings.NewReader(""), &strings.Builder{}); ok || exit != 1 {
		t.Fatalf("missing input: exit %d ok %v, want 1 false", exit, ok)
	}
}
