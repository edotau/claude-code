package tui

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// stdinReader is the one bufio.Reader over os.Stdin for every line-based read in this package, matching
// configure's shared-reader pattern: a fresh bufio.Reader per call buffers ahead of the line it returns,
// then discards that look-ahead on the next call, losing bytes the user already typed.
var stdinReader = bufio.NewReader(os.Stdin)

// Confirm prints "question [Y/n] " (or "[y/N]" when def is false) and reads one line from stdin;
// blank keeps def, a leading y/Y is true, a leading n/N is false, anything else keeps def. Reads
// directly from the controlling terminal — call only once the caller has confirmed stdin is a real
// TTY (configure's Options.Interactive is already that gate); never against a substituted reader.
func Confirm(question string, def bool) bool {
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	fmt.Fprintf(os.Stdout, "%s %s ", question, hint)
	line, _ := stdinReader.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	switch {
	case line == "":
		return def
	case strings.HasPrefix(line, "y"):
		return true
	case strings.HasPrefix(line, "n"):
		return false
	default:
		return def
	}
}

// Secret prints prompt and reads one line with local echo off, so a pasted key never lands in the scrollback or
// shell history. A non-nil preview redraws a masked line under the prompt as input arrives, so a paste shows on paste.
// Falls back to a plain read when stdin is not a terminal.
func Secret(prompt string, preview func(string) string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		fmt.Fprint(os.Stdout, prompt)
		line, err := stdinReader.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}
	var onChange func([]byte)
	if preview == nil {
		fmt.Fprint(os.Stdout, prompt)
	} else {
		fmt.Fprintln(os.Stdout, strings.TrimRight(prompt, " "))
		onChange = func(b []byte) {
			if fmt.Fprint(os.Stdout, "\r\x1b[K"); len(b) > 0 {
				fmt.Fprintf(os.Stdout, "  pasted  %s", preview(string(b)))
			}
		}
	}
	b, err := readPassword(fd, onChange)
	fmt.Fprintln(os.Stdout)
	return string(b), err
}

// StdinIsTerminal reports whether stdin is a real terminal (a /dev/null char device is not).
func StdinIsTerminal() bool { return isTerminal(int(os.Stdin.Fd())) }

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
	t, err := Open()
	if err != nil {
		return Confirm(question, def)
	}
	defer t.Close()
	hint := "[Y/n]"
	if !def {
		hint = "[y/N]"
	}
	fmt.Fprintf(os.Stdout, "%s %s · Esc cancel ", question, hint)
	for k := range t.Keys() {
		if answer, done := confirmKey(k, def); done {
			fmt.Fprintln(os.Stdout, map[bool]string{true: "yes", false: "no"}[answer])
			return answer
		}
	}
	return false
}
