package tui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/proc"
)

// stdinReader is the one bufio.Reader over os.Stdin for every line-based read in this package, matching
// configure's shared-reader pattern: a fresh bufio.Reader per call buffers ahead of the line it returns,
// then discards that look-ahead on the next call, losing bytes the user already typed.
var stdinReader = bufio.NewReader(os.Stdin)

// Stdin is that shared reader, for line reads outside tui (menu's prompts) that must not drop its look-ahead.
func Stdin() *bufio.Reader { return stdinReader }

// Confirm prints "question [Y/n] " (or "[y/N]" when def is false) and reads one line from stdin;
// blank keeps def, a leading y/Y is true, a leading n/N is false, anything else keeps def. Reads
// directly from the controlling terminal — call only once the caller has confirmed stdin is a real
// TTY (configure's Options.Interactive is already that gate); never against a substituted reader.
func Confirm(question string, def bool) bool {
	fmt.Fprintf(os.Stdout, "%s %s ", question, yesNoHint(def))
	line, err := stdinReader.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return false // EOF or a dead stdin is a decline
	}
	return ParseYesNo(line, def)
}

// ParseYesNo reads a y/n answer: a leading y/Y is true, a leading n/N is false, anything else (blank too) is def.
func ParseYesNo(line string, def bool) bool {
	if l := strings.ToLower(strings.TrimSpace(line)); l != "" && (l[0] == 'y' || l[0] == 'n') {
		return l[0] == 'y'
	}
	return def
}

// yesNoHint is the "[Y/n]" or "[y/N]" suffix that marks def.
func yesNoHint(def bool) string {
	if def {
		return "[Y/n]"
	}
	return "[y/N]"
}

// Secret prints prompt and reads one line with local echo off, so a pasted key never lands in the scrollback or
// shell history. A non-nil preview redraws a masked line under the prompt as input arrives, so a paste shows on paste.
// Falls back to a plain read when stdin is not a terminal.
func Secret(prompt string, preview func(string) string) (string, error) {
	return SecretHint("", prompt, preview)
}

// SecretHint is Secret with a hint line above the input that is erased on the first input byte (terminals only).
func SecretHint(hint, prompt string, preview func(string) string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !proc.IsTerminalFD(fd) {
		return secretFromPipe(hint, prompt)
	}
	hintShown := hint != "" && StdoutIsTerminal()
	printSecretPrompt(hint, prompt, hintShown, preview != nil)
	onChange := func(b []byte) {
		if hintShown && len(b) > 0 {
			fmt.Fprint(os.Stdout, "\x1b[1A\x1b[M") // up onto the hint line, delete it: the input line moves up
			hintShown = false
		}
		if preview != nil {
			if fmt.Fprint(os.Stdout, "\r\x1b[K"); len(b) > 0 {
				fmt.Fprintf(os.Stdout, "  pasted  %s", preview(string(b)))
			}
		}
	}
	b, err := readPassword(fd, onChange)
	fmt.Fprintln(os.Stdout)
	return string(b), err
}

// secretFromPipe is SecretHint off a terminal: a plain line read.
func secretFromPipe(hint, prompt string) (string, error) {
	if hint != "" {
		fmt.Fprintln(os.Stdout, hint)
	}
	fmt.Fprint(os.Stdout, prompt)
	line, err := stdinReader.ReadString('\n')
	if err == io.EOF && line != "" {
		err = nil // a final line without a trailing newline is still a good secret
	}
	return strings.TrimRight(line, "\r\n"), err
}

// printSecretPrompt writes the hint and prompt; the hint goes above an inline prompt, under the line when previewing.
func printSecretPrompt(hint, prompt string, hintShown, previewing bool) {
	if !previewing {
		if hintShown {
			fmt.Fprintln(os.Stdout, hint)
		}
		fmt.Fprint(os.Stdout, prompt)
		return
	}
	fmt.Fprintln(os.Stdout, strings.TrimRight(prompt, " "))
	if hintShown {
		fmt.Fprintln(os.Stdout, hint)
	}
}

// StdinIsTerminal reports whether stdin is a real terminal (a /dev/null char device is not).
func StdinIsTerminal() bool { return proc.IsTerminalFD(int(os.Stdin.Fd())) }

// StdoutIsTerminal reports whether stdout is a real terminal (a /dev/null char device is not).
func StdoutIsTerminal() bool { return proc.IsTerminalFD(int(os.Stdout.Fd())) }

// confirmKey maps one keypress to an answer: y yes; n, Esc, q or Ctrl-C no; Enter the default; done=false waits.
func confirmKey(k Key, def bool) (answer, done bool) {
	switch {
	case k.Kind == KeyRune && (k.Rune == 'y' || k.Rune == 'Y'):
		return true, true
	case k.Kind == KeyRune && (k.Rune == 'n' || k.Rune == 'N' || k.Rune == 'q'), k.Kind == KeyEsc, k.Kind == KeyCtrlC:
		return false, true
	case k.Kind == KeyEnter:
		return def, true
	}
	return false, false
}

// ConfirmKey asks a [y/N]-style question answered by a single keypress (Esc cancels at once); it falls back to the
// line-based Confirm when stdin is not a terminal.
func ConfirmKey(question string, def bool) bool {
	t, err := openTerminal()
	if err != nil {
		return Confirm(question, def)
	}
	defer t.Close()
	fmt.Fprintf(os.Stdout, "%s %s · Esc cancel ", question, yesNoHint(def))
	for k := range t.Keys() {
		if answer, done := confirmKey(k, def); done {
			fmt.Fprint(os.Stdout, map[bool]string{true: "yes", false: "no"}[answer], "\r\n") // raw mode: no OPOST
			return answer
		}
	}
	return false
}

// errCanceled is what readPassword returns when Esc, Ctrl-C or Ctrl-D ends the read before Enter.
var errCanceled = errors.New("tui: input canceled")

// feedPassword applies one read's bytes to line, skipping escape sequences; done once Enter or a cancel ends it.
func feedPassword(line, in []byte, esc *escState) (_ []byte, done bool, err error) {
	for _, c := range in {
		if esc.skip(c) {
			continue
		}
		l, done, kerr := editLine(line, c)
		if line = l; done {
			return line, true, kerr
		}
	}
	return line, false, nil
}

// editLine applies one typed byte to line; done is true on Enter (err nil) or Ctrl-C/Ctrl-D (errCanceled).
func editLine(line []byte, c byte) (_ []byte, done bool, err error) {
	switch {
	case c == '\n' || c == '\r':
		return line, true, nil
	case c == 0x03 || c == 0x04:
		return nil, true, errCanceled
	case c == 0x7f || c == 0x08:
		return dropLastRune(line), false, nil
	case c == 0x15:
		return line[:0], false, nil
	case c >= 0x20:
		return append(line, c), false, nil
	}
	return line, false, nil
}

// dropLastRune trims the line's last whole UTF-8 rune, so Backspace over a multibyte character removes all of it.
func dropLastRune(line []byte) []byte {
	_, size := utf8.DecodeLastRune(line)
	return line[:len(line)-size]
}

// escState tracks an ANSI escape sequence across reads so a bracketed-paste marker (ESC[200~, ESC[201~) or an
// arrow key is dropped instead of leaking "[200~" into the secret.
type escState int

const (
	escNone escState = iota
	escSeen
	escCSI
	escSS3
)

// skip reports whether c belongs to an escape sequence and is to be ignored.
func (s *escState) skip(c byte) bool {
	switch *s {
	case escSeen:
		*s = escNone
		if c == '[' {
			*s = escCSI
			return true
		}
		if c == 'O' {
			*s = escSS3
			return true
		}
	case escSS3: // ESC O x: application-mode arrow or function key, one final byte
		*s = escNone
		return true
	case escCSI:
		if c >= 0x40 && c <= 0x7e {
			*s = escNone
		}
		return true
	default:
		if c == 0x1b {
			*s = escSeen
			return true
		}
	}
	return false
}
