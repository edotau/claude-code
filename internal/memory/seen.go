package memory

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// seenMaxAge prunes per-session seen sets; a session idle this long has long since compacted or ended.
const seenMaxAge = 7 * 24 * time.Hour

// BlockKey identifies a block by its whitespace-normalized text.
func BlockKey(block string) string {
	h := fnv.New64a()
	h.Write([]byte(strings.Join(strings.Fields(block), " ")))
	return strconv.FormatUint(h.Sum64(), 16)
}

func seenPath(sessionID string) string {
	id := filepath.Base(sessionID)
	if id == "" || id == "." || id == ".." || id == string(filepath.Separator) {
		return ""
	}
	return filepath.Join(StateDir(), "recall-seen", id)
}

// ResetSeen restarts sessionID's seen set with every corpus block the SessionStart surface carried in full.
func ResetSeen(sessionID, root, surface string) {
	p := seenPath(sessionID)
	if p == "" {
		return
	}
	pruneSeen(filepath.Dir(p))
	flat := strings.Join(strings.Fields(surface), " ")
	var b strings.Builder
	for _, h := range corpusBlocks(root) {
		if norm := strings.Join(strings.Fields(h.Block), " "); norm != "" && strings.Contains(flat, norm) {
			b.WriteString(BlockKey(h.Block) + "\n")
		}
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = paths.AtomicWrite(p, []byte(b.String()), 0o600)
}

// SeenBlocks returns sessionID's seen set: blocks already in the conversation's context.
func SeenBlocks(sessionID string) map[string]bool {
	out := map[string]bool{}
	p := seenPath(sessionID)
	if p == "" {
		return out
	}
	data, _ := os.ReadFile(p)
	for _, k := range strings.Fields(string(data)) {
		out[k] = true
	}
	return out
}

// MarkSeen appends the injected hits to sessionID's seen set; fail-open.
func MarkSeen(sessionID string, hits []SearchHit) {
	p := seenPath(sessionID)
	if p == "" || len(hits) == 0 {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	for _, h := range hits {
		_, _ = f.WriteString(BlockKey(h.Block) + "\n")
	}
}

func pruneSeen(dir string) {
	ents, _ := os.ReadDir(dir)
	cutoff := time.Now().Add(-seenMaxAge)
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
