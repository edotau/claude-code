package ask

import (
	"slices"
	"testing"
)

func TestLastLinesDropsTrailingFooters(t *testing.T) {
	text := "one\ntwo\nthree\n\nask: ran on x · y\nask: dispatch 1-x\nsession: abc\nresume: x --resume abc\n"
	if got, want := lastLines(text, 2), []string{"two", "three"}; !slices.Equal(got, want) {
		t.Errorf("lastLines = %q, want %q", got, want)
	}
	if got := lastLines("ask: dispatch 1\n", 3); got == nil || len(got) != 0 {
		t.Errorf("footer-only text = %#v, want empty non-nil", got)
	}
	if got, want := lastLines("ask: mid\nlast\n", 5), []string{"ask: mid", "last"}; !slices.Equal(got, want) {
		t.Errorf("non-trailing footer-like line = %q, want %q", got, want)
	}
}

func TestLastLinesDropsTraceLinesWhenAnswerExists(t *testing.T) {
	text := "▸ assistant: draft\n▸ result: step finished\nalpha\nbeta\n▸ result: tail\nsession: s\n"
	if got, want := lastLines(text, 10), []string{"alpha", "beta"}; !slices.Equal(got, want) {
		t.Errorf("lastLines = %q, want only answer lines %q", got, want)
	}
	only := "▸ result: step\n▸ assistant: one\n▸ result: step\n▸ assistant: two\n"
	if got, want := lastLines(only, 10), []string{"▸ assistant: one", "▸ assistant: two"}; !slices.Equal(got, want) {
		t.Errorf("trace-only log = %q, want only its assistant lines %q", got, want)
	}
	bare := []string{"▸ result: a", "▸ result: b"}
	if got := lastLines("▸ result: a\n▸ result: b\n", 10); !slices.Equal(got, bare) {
		t.Errorf("log with no answer = %q, want %q", got, bare)
	}
}
