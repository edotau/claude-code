package textutil

import (
	"io"
	"strings"
)

// ErrWriter forwards writes to W and keeps the first error in Err, so a caller checks once after a run of prints.
type ErrWriter struct {
	W   io.Writer
	Err error
}

func (e *ErrWriter) Write(p []byte) (int, error) {
	if e.Err != nil {
		return 0, e.Err
	}
	n, err := e.W.Write(p)
	e.Err = err
	return n, err
}

// LastLine returns the last non-empty line of s, trimmed.
func LastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, " \t\r\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
