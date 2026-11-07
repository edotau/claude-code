package tui

import (
	"bufio"
	"io"
	"os"
	"strings"
	"testing"
)

// TestConfirmKey: y answers yes; n, q, Esc and Ctrl-C cancel at once; Enter takes the default; other keys wait.
func TestConfirmKey(t *testing.T) {
	cases := []struct {
		k         Key
		def, want bool
		done      bool
	}{
		{Key{Kind: KeyRune, Rune: 'y'}, false, true, true},
		{Key{Kind: KeyRune, Rune: 'n'}, true, false, true},
		{Key{Kind: KeyEsc}, true, false, true},
		{Key{Kind: KeyCtrlC}, true, false, true},
		{Key{Kind: KeyEnter}, false, false, true},
		{Key{Kind: KeyEnter}, true, true, true},
		{Key{Kind: KeyRune, Rune: 'x'}, false, false, false},
	}
	for _, c := range cases {
		if got, done := confirmKey(c.k, c.def); got != c.want || done != c.done {
			t.Errorf("confirmKey(%+v, def=%v) = %v,%v want %v,%v", c.k, c.def, got, done, c.want, c.done)
		}
	}
}

func withStdin(t *testing.T, s string) {
	t.Helper()
	orig := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader(s))
	t.Cleanup(func() { stdinReader = orig })
}

// TestSecretFallbackKeepsUnterminatedLine: piped "sk-abc" with no newline is a good secret, not an io.EOF error.
func TestSecretFallbackKeepsUnterminatedLine(t *testing.T) {
	withStdin(t, "sk-abc")
	got, err := Secret("key: ", nil)
	if err != nil || got != "sk-abc" {
		t.Fatalf("Secret = %q, %v; want sk-abc, nil", got, err)
	}
}

// TestSecretFallbackEmptyStdinStillErrors: EOF with no data remains an error.
func TestSecretFallbackEmptyStdinStillErrors(t *testing.T) {
	withStdin(t, "")
	if _, err := Secret("key: ", nil); err != io.EOF {
		t.Fatalf("err = %v; want io.EOF", err)
	}
}

// TestConfirmEOFDeclines: a closed stdin declines even when the default is yes; a blank line takes the default.
func TestConfirmEOFDeclines(t *testing.T) {
	withStdin(t, "")
	if Confirm("ok?", true) {
		t.Error("Confirm at EOF returned true, want false")
	}
	withStdin(t, "\n")
	if !Confirm("ok?", true) {
		t.Error("Confirm on blank line returned false, want default true")
	}
	withStdin(t, "y")
	if !Confirm("ok?", false) {
		t.Error("Confirm on unterminated y returned false, want true")
	}
}

func TestYesNoHint(t *testing.T) {
	if yesNoHint(true) != "[Y/n]" || yesNoHint(false) != "[y/N]" {
		t.Errorf("hints = %q, %q", yesNoHint(true), yesNoHint(false))
	}
}

// TestSecretHintPipedPrintsPlainHint: with stdin not a terminal the hint is a plain line above the prompt, no ANSI.
func TestSecretHintPipedPrintsPlainHint(t *testing.T) {
	withStdin(t, "abc\n")
	r, w, _ := os.Pipe()
	orig := os.Stdout
	os.Stdout = w
	got, err := SecretHint("empty keeps x", "id: ", nil)
	os.Stdout = orig
	w.Close()
	out, _ := io.ReadAll(r)
	if err != nil || got != "abc" || string(out) != "empty keeps x\nid: " {
		t.Fatalf("got %q, %v, out %q", got, err, out)
	}
}
