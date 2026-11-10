package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// readFile is os.ReadFile for typed bank files; a var so tests can count how often one file is read.
var readFile = os.ReadFile

// indexLineMax keeps routing lines scannable; detail lives in the entry itself.
const indexLineMax = 120

// entryBlocks splits by the grammar the file NAME implies: dated logs on "## " only, others also on "- ".
func entryBlocks(base, body string) (header string, blocks []string) {
	if base == "decisionLog.md" || base == "sessionHistory.md" {
		return splitHeadingBlocks(body)
	}
	return splitOn(body, func(ln string) bool {
		return strings.HasPrefix(ln, "- ") || strings.HasPrefix(ln, "## ")
	})
}

// splitHeadingBlocks splits a newest-first log into its header and "## " entries (bodies may hold bullets).
func splitHeadingBlocks(body string) (header string, blocks []string) {
	return splitOn(body, func(ln string) bool { return strings.HasPrefix(ln, "## ") })
}

// nextFence returns the open fence's marker run after ln: an opener sets it, only a bare run of its character closes.
func nextFence(open, ln string) string {
	t := strings.TrimSpace(ln)
	if open != "" {
		if fenceCloses(t, open) {
			return ""
		}
		return open
	}
	if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
		return fenceRun(t)
	}
	return ""
}

// fenceRun is the leading run of fence characters in a trimmed fence line ("```" for "```go").
func fenceRun(trimmed string) string {
	n := 0
	for n < len(trimmed) && trimmed[n] == trimmed[0] {
		n++
	}
	return trimmed[:n]
}

// fenceCloses reports whether trimmed is a bare run of open's character at least as long as open.
func fenceCloses(trimmed, open string) bool {
	return len(trimmed) >= len(open) && strings.Trim(trimmed, open[:1]) == ""
}

// splitOn is the fence-aware splitter: a marker-shaped line inside a fence never starts a block.
func splitOn(body string, isMarker func(string) bool) (header string, blocks []string) {
	var hdr, cur []string
	inBlock := false
	fence := "" // marker run of the open fence, "" outside one
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.TrimRight(strings.Join(cur, "\n"), "\n"))
			cur = nil
		}
	}
	for ln := range strings.SplitSeq(body, "\n") {
		if fence == "" && isMarker(ln) {
			flush()
			inBlock = true
		}
		fence = nextFence(fence, ln)
		if inBlock {
			cur = append(cur, ln)
		} else {
			hdr = append(hdr, ln)
		}
	}
	flush()
	return strings.TrimRight(strings.Join(hdr, "\n"), "\n"), blocks
}

// rejoin reassembles a bank file from its header and kept blocks.
func rejoin(header string, blocks []string) string {
	var b strings.Builder
	if header != "" {
		b.WriteString(header + "\n")
	}
	for _, blk := range blocks {
		b.WriteString("\n" + blk + "\n")
	}
	out := strings.TrimLeft(b.String(), "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// appendBlocks appends blocks to path, creating it with a minimal header when absent.
func appendBlocks(path string, blocks []string) error {
	existing, err := readFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err // never overwrite an archive we could not read
	}
	return paths.AtomicWrite(path, []byte(appendedText(path, string(existing), blocks)), 0o600)
}

// appendedText is body with blocks appended, under a minimal header when body is blank.
func appendedText(path, body string, blocks []string) string {
	if strings.TrimSpace(body) == "" {
		body = "# " + strings.TrimSuffix(filepath.Base(path), ".md") + " (archived)\n"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	var b strings.Builder
	b.WriteString(body)
	for _, block := range blocks {
		b.WriteByte('\n')
		b.WriteString(block)
		b.WriteByte('\n')
	}
	return b.String()
}

// entriesWithPrefix returns every line starting with prefix, prefix stripped and truncated, in file order.
func entriesWithPrefix(path, prefix string) ([]string, error) {
	data, err := readIfExists(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			e := truncateLine(stripID(rest))
			if id := blockID(rest); id != "" {
				e += " [" + id + "]"
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// truncateLine caps s at indexLineMax bytes, cutting at a word boundary, rune-safe.
func truncateLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= indexLineMax {
		return s
	}
	cut := strings.ToValidUTF8(s[:indexLineMax], "")
	if i := strings.LastIndexByte(cut, ' '); i > indexLineMax/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// lineCount counts lines in s (0 for empty) — the budget unit promote/archive split on.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}
