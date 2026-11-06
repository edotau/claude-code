package dialog

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func shellOn(script string) (*Shell, *bytes.Buffer) {
	var out bytes.Buffer
	return &Shell{Out: &out, In: bufio.NewReader(strings.NewReader(script))}, &out
}

// Given a page with a counting row and Done, When 1 is entered twice then 2, Then the row ran twice and Run reports done.
func TestRunKeepsPageUntilRowDone(t *testing.T) {
	s, out := shellOn("1\n1\n2\n")
	n := 0
	done := s.Run("T", func() []Row {
		return []Row{{Name: "a", Detail: "x", Run: func() bool { n++; return false }}, {Name: "Done", Run: func() bool { return true }}}
	})
	if !done || n != 2 || !strings.Contains(out.String(), "1) a") {
		t.Fatalf("done=%v n=%d out=%q", done, n, out.String())
	}
}

// Given a blank or out-of-range answer, Then Run returns false and no row runs.
func TestRunDismissed(t *testing.T) {
	for _, in := range []string{"\n", "9\n", ""} {
		s, _ := shellOn(in)
		if s.Run("T", func() []Row { return []Row{{Name: "a", Run: func() bool { t.Fatal("ran"); return true }}} }) {
			t.Fatalf("%q reported done", in)
		}
	}
}

func TestPaintAndMark(t *testing.T) {
	s, out := shellOn("\n")
	s.Paint = strings.ToUpper
	s.Pick("hi", [][2]string{{"a", "b"}})
	if !strings.Contains(out.String(), "HI\n") || Mark(true) != " (current)" || Mark(false) != "" {
		t.Fatalf("%q", out.String())
	}
}

// Given two lines typed ahead on the terminal, When Line runs twice, Then the second call sees the first call's look-ahead.
func TestLineTTYKeepsLookAhead(t *testing.T) {
	orig, r := lineReader, bufio.NewReader(strings.NewReader("one\ntwo\n"))
	lineReader = func() *bufio.Reader { return r }
	t.Cleanup(func() { lineReader = orig })
	var out bytes.Buffer
	s := &Shell{Out: &out, TTY: true}
	if a, b := s.Line("> "), s.Line("> "); a != "one" || b != "two" {
		t.Fatalf("got %q then %q", a, b)
	}
}
