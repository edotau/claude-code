package textutil

import (
	"errors"
	"strings"
	"testing"
)

type failWriter struct{ calls int }

func (f *failWriter) Write([]byte) (int, error) { f.calls++; return 0, errors.New("boom") }

func TestErrWriterKeepsFirstError(t *testing.T) {
	fw := &failWriter{}
	ew := &ErrWriter{W: fw}
	ew.Write([]byte("a"))
	ew.Write([]byte("b"))
	if ew.Err == nil || fw.calls != 1 {
		t.Fatalf("err=%v calls=%d, want first error kept and later writes skipped", ew.Err, fw.calls)
	}
	var sb strings.Builder
	ok := &ErrWriter{W: &sb}
	ok.Write([]byte("x"))
	if ok.Err != nil || sb.String() != "x" {
		t.Fatalf("err=%v out=%q", ok.Err, sb.String())
	}
}

func TestLastLine(t *testing.T) {
	for in, want := range map[string]string{"": "", "a": "a", "a\nb\n\n": "b", "a\n  b \r\n": "b", "https://x/1\n": "https://x/1"} {
		if got := LastLine(in); got != want {
			t.Errorf("LastLine(%q)=%q want %q", in, got, want)
		}
	}
}
