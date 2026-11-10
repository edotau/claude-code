package memory

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// indexLineMax keeps routing lines scannable; detail lives in the entry itself.
const indexLineMax = 120

// EntryBlocks splits by the grammar the file NAME implies: dated logs on "## " only, others also on "- ".
func EntryBlocks(base, body string) (header string, blocks []string) {
	if base == "decisionLog.md" || base == "sessionHistory.md" {
		return SplitHeadingBlocks(body)
	}
	return splitOn(body, func(ln string) bool {
		return strings.HasPrefix(ln, "- ") || strings.HasPrefix(ln, "## ")
	})
}

// SplitHeadingBlocks splits a newest-first log into its header and "## " entries (bodies may hold bullets).
func SplitHeadingBlocks(body string) (header string, blocks []string) {
	return splitOn(body, func(ln string) bool { return strings.HasPrefix(ln, "## ") })
}

// fenceToggle reports whether ln opens or closes a ``` / ~~~ fence.
func fenceToggle(ln string) bool {
	t := strings.TrimSpace(ln)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
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

// Rejoin reassembles a bank file from its header and kept blocks.
func Rejoin(header string, blocks []string) string {
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

// AppendBlocks appends blocks to path, creating it with a minimal header when absent.
func AppendBlocks(path string, blocks []string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err // never overwrite an archive we could not read
	}
	body := string(existing)
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
	return paths.AtomicWrite(path, []byte(b.String()), 0o600)
}

// EntriesWithPrefix returns every line starting with prefix, prefix stripped and truncated, in file order.
func EntriesWithPrefix(path, prefix string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			e := truncateLine(StripID(rest))
			if id := BlockID(rest); id != "" {
				e += " [" + id + "]"
			}
			out = append(out, e)
		}
	}
	return out
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

// LineCount counts lines in s (0 for empty) — the budget unit promote/archive split on.
func LineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}
