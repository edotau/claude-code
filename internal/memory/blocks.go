package memory

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

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

// fenceToggle reports whether ln opens or closes a ``` / ~~~ fence.
func fenceToggle(ln string) bool {
	t := strings.TrimSpace(ln)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// splitOn is the fence-aware splitter: a marker-shaped line inside a fence never starts a block.
func splitOn(body string, isMarker func(string) bool) (header string, blocks []string) {
	var hdr, cur []string
	inBlock, inFence := false, false
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.TrimRight(strings.Join(cur, "\n"), "\n"))
			cur = nil
		}
	}
	for _, ln := range strings.Split(body, "\n") {
		if !inFence && isMarker(ln) {
			flush()
			inBlock = true
		}
		if fenceToggle(ln) {
			inFence = !inFence
		}
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
	existing, _ := os.ReadFile(path)
	body := string(existing)
	if strings.TrimSpace(body) == "" {
		body = "# " + strings.TrimSuffix(filepath.Base(path), ".md") + " (archived)\n"
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	for _, b := range blocks {
		body += "\n" + b + "\n"
	}
	return paths.AtomicWrite(path, []byte(body), 0o600)
}

// entriesWithPrefix returns every line starting with prefix, prefix stripped and truncated, in file order.
func entriesWithPrefix(path, prefix string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			out = append(out, truncateLine(rest))
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

// NewestHeadingBlocks returns up to n "## " blocks from the top of a newest-first file; nil when absent.
func NewestHeadingBlocks(path string, n int) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	_, blocks := splitHeadingBlocks(string(data))
	if len(blocks) > n {
		blocks = blocks[:n]
	}
	return blocks
}
