package theme

import (
	"bytes"
	"strings"
)

// mergeGhostty returns dest with each overlay key's lines replacing that key's dest lines at its first
// position; keys new to dest are appended in overlay order; overlay comments are dropped.
// Indexed palette overlays ("palette = N=#rrggbb") merge per index and keep unmentioned dest entries.
func mergeGhostty(dest, overlay []byte) []byte {
	set, order := overlayKeys(overlay)
	byIdx, indexed := paletteByIndex(set["palette"])
	var out [][]byte
	done := map[string]bool{}
	used := map[string]bool{}
	for _, line := range splitLines(dest) {
		key := parseKey(line)
		lines, ok := set[key]
		if indexed && key == "palette" {
			idx := paletteIndex(line)
			if r, hit := byIdx[idx]; hit && idx != "" {
				line, used[idx] = r, true
			}
			out = append(out, line)
			done[key] = true
			continue
		}
		switch {
		case key == "" || !ok:
			out = append(out, line)
		case !done[key]:
			out = append(out, lines...)
			done[key] = true
		}
	}
	for _, key := range order {
		switch {
		case indexed && key == "palette":
			for _, l := range set[key] {
				if !used[paletteIndex(l)] {
					out = append(out, l)
				}
			}
		case !done[key]:
			out = append(out, set[key]...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return append(bytes.Join(out, []byte("\n")), '\n')
}

// paletteIndex returns N of a "palette = N=color" line, or "" when the value has no index.
func paletteIndex(line []byte) string {
	_, val, _ := strings.Cut(string(line), "=")
	idx, _, ok := strings.Cut(val, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(idx)
}

// paletteByIndex maps index to line, and reports whether every line carries an index.
func paletteByIndex(lines [][]byte) (map[string][]byte, bool) {
	m := map[string][]byte{}
	for _, l := range lines {
		idx := paletteIndex(l)
		if idx == "" {
			return nil, false
		}
		m[idx] = l
	}
	return m, len(m) > 0
}

// overlayKeys groups the overlay's setting lines by key, with the keys in first-seen order.
func overlayKeys(overlay []byte) (set map[string][][]byte, order []string) {
	set = map[string][][]byte{}
	for _, line := range splitLines(overlay) {
		key := parseKey(line)
		if key == "" {
			continue
		}
		if _, ok := set[key]; !ok {
			order = append(order, key)
		}
		set[key] = append(set[key], line)
	}
	return set, order
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
