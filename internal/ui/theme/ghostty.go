package theme

import (
	"bytes"
	"strings"
)

// mergeGhostty returns dest with each overlay key's lines replacing that key's dest lines at its first
// position; keys new to dest are appended in overlay order; overlay comments are dropped.
func mergeGhostty(dest, overlay []byte) []byte {
	set := map[string][][]byte{}
	var order []string
	for _, line := range splitLines(overlay) {
		if key := parseKey(line); key != "" {
			if _, ok := set[key]; !ok {
				order = append(order, key)
			}
			set[key] = append(set[key], line)
		}
	}
	var out [][]byte
	done := map[string]bool{}
	for _, line := range splitLines(dest) {
		key := parseKey(line)
		lines, ok := set[key]
		switch {
		case key == "" || !ok:
			out = append(out, line)
		case !done[key]:
			out = append(out, lines...)
			done[key] = true
		}
	}
	for _, key := range order {
		if !done[key] {
			out = append(out, set[key]...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return append(bytes.Join(out, []byte("\n")), '\n')
}

// splitLines splits on newlines without the phantom empty line a trailing newline leaves.
func splitLines(b []byte) [][]byte {
	b = bytes.TrimRight(b, "\n")
	if len(b) == 0 {
		return nil
	}
	return bytes.Split(b, []byte("\n"))
}

// parseKey returns the key of a "key = value" line, or "" for blanks, comments and non-settings.
func parseKey(line []byte) string {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	key, _, ok := strings.Cut(trimmed, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(key)
}
