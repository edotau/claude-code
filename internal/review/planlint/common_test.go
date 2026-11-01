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
	text, source, ok := readInput("-", r, &stderr)
	if ok || text != "" || source != "" {
		t.Fatalf("got (%q, %q, %v), want empty and ok=false", text, source, ok)
	}
	if !strings.Contains(stderr.String(), "broken pipe") {
		t.Fatalf("stderr %q does not name the read error", stderr.String())
	}
}

func TestReadInputStdinOK(t *testing.T) {
	text, source, ok := readInput("-", strings.NewReader("hello"), &strings.Builder{})
	if !ok || text != "hello" || source != "stdin" {
		t.Fatalf("got (%q, %q, %v)", text, source, ok)
	}
}
