package tui

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

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
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
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

// Secret prints prompt and reads one line with local echo off, so a pasted key never lands in the
// scrollback or shell history. Falls back to a plain read when stdin is not a terminal.
func Secret(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}
	b, err := readPassword(fd)
	fmt.Fprintln(os.Stdout)
	return string(b), err
}
