package textutil

import (
	"io"
	"strconv"
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

// SetOf is items as a set.
func SetOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, it := range items {
		m[it] = true
	}
	return m
}

// LastLine returns the last non-empty line of s, trimmed.
func LastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, " \t\r\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// CompactCount spells n as 950, 94.4k, 200k or 1.2M: one decimal, a trailing ".0" dropped, millions from 999_950.
func CompactCount(n int64) string {
	switch {
	case n >= 999_950: // from 999_950 %.1fk would print 1000.0k
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64), ".0") + "M"
	case n >= 1000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64), ".0") + "k"
	}
	return strconv.FormatInt(n, 10)
}
