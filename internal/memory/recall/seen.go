package recall

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory/bank"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

// seenMaxAge prunes per-session seen sets; a session idle this long has long since compacted or ended.
const seenMaxAge = 7 * 24 * time.Hour

// sessionIDShape matches a Claude Code session id (UUID), rejecting probe/test payloads like "probe-x".
var sessionIDShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// LooksLikeSessionID reports whether id has the UUID shape Claude Code assigns real sessions.
func LooksLikeSessionID(id string) bool { return sessionIDShape.MatchString(id) }

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
	return filepath.Join(memstore.StateDir(), "recall-seen", id)
}

// ResetSeen restarts sessionID's seen set with every corpus block the SessionStart surface carried in full.
func ResetSeen(sessionID, root, surface string) {
	if !LooksLikeSessionID(sessionID) {
		return
	}
	p := seenPath(sessionID)
	if p == "" {
		return
	}
	pruneSeen(filepath.Dir(p))
	flat := strings.Join(strings.Fields(surface), " ")
	hits, err := bank.CorpusBlocks(root)
	if err != nil {
		return // fail-open: no corpus read, so the seen set is left as it was
	}
	var b strings.Builder
	for _, h := range hits {
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
	for k := range strings.FieldsSeq(string(data)) {
		out[k] = true
	}
	return out
}

// MarkSeen appends the injected hits to sessionID's seen set; fail-open.
func MarkSeen(sessionID string, hits []bank.SearchHit) {
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
	cutoff := time.Now().Add(-seenMaxAge)
	for _, path := range []string{dir, memstore.StateDir()} {
		ents, _ := os.ReadDir(path)
		for _, e := range ents {
			if !e.Type().IsRegular() || path != dir && !prunableSessionState(e.Name()) && !staleSessionRoot(path, e.Name(), cutoff) {
				continue
			}
			if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(path, e.Name()))
			}
		}
	}
}

func prunableSessionState(name string) bool {
	for _, prefix := range []string{"count-", "stamp-", "watch-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	for _, prefix := range []string{"update-", "harvest-", "dream-", "catchup-"} {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".log") {
			return true
		}
	}
	return false
}

// staleSessionRoot reports a root-<id>[.lock] file whose session has no count-/stamp-/watch- file newer than cutoff.
func staleSessionRoot(dir, name string, cutoff time.Time) bool {
	id, ok := strings.CutPrefix(name, "root-")
	if !ok {
		return false
	}
	id = strings.TrimSuffix(id, ".lock")
	for _, prefix := range []string{"count-", "stamp-", "watch-"} {
		if fi, err := os.Stat(filepath.Join(dir, prefix+id)); err == nil && !fi.ModTime().Before(cutoff) {
			return false
		}
	}
	return true
}
