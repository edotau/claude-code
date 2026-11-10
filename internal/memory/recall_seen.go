package memory

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

// seenMaxAge prunes per-session seen sets; a session idle this long has long since compacted or ended.
const seenMaxAge = 7 * 24 * time.Hour

// sessionIDShape matches a Claude Code session id (UUID), rejecting probe/test payloads like "probe-x".
var sessionIDShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// LooksLikeSessionID reports whether id has the UUID shape Claude Code assigns real sessions.
func LooksLikeSessionID(id string) bool { return sessionIDShape.MatchString(id) }

// blockKey identifies a block by its whitespace-normalized text.
func blockKey(block string) string {
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
	if !LooksLikeSessionID(sessionID) {
		return
	}
	p := seenPath(sessionID)
	if p == "" {
		return
	}
	pruneSeen(filepath.Dir(p))
	flat := strings.Join(strings.Fields(surface), " ")
	hits, err := corpusBlocks(root)
	if err != nil {
		return // fail-open: no corpus read, so the seen set is left as it was
	}
	var b strings.Builder
	for _, h := range hits {
		if norm := strings.Join(strings.Fields(h.Block), " "); norm != "" && strings.Contains(flat, norm) {
			b.WriteString(blockKey(h.Block) + "\n")
		}
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	_ = paths.AtomicWrite(p, []byte(b.String()), 0o600)
}

// seenBlocks returns sessionID's seen set: blocks already in the conversation's context.
func seenBlocks(sessionID string) map[string]bool {
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
		_, _ = f.WriteString(blockKey(h.Block) + "\n")
	}
}

func pruneSeen(dir string) {
	cutoff := time.Now().Add(-seenMaxAge)
	for _, path := range []string{dir, StateDir()} {
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

// sessionRootLockWait bounds how long a hook waits for a racing repair to finish.
const sessionRootLockWait = 5 * time.Second

func sessionRootPath(sessionID string) string {
	return filepath.Join(StateDir(), "root-"+filepath.Base(sessionID))
}

// SessionRoot uses the launch repository when available, otherwise the hook payload cwd.
func SessionRoot(sessionID, cwd string) string {
	if !LooksLikeSessionID(sessionID) || IsUpdateChild() {
		return cwd
	}
	data, err := os.ReadFile(sessionRootPath(sessionID))
	root := strings.TrimSpace(string(data))
	if err != nil || !filepath.IsAbs(root) {
		return cwd
	}
	return filepath.Clean(root)
}

// sessionRootFor is cwd's git toplevel, else its store origin, else its absolute path.
func sessionRootFor(cwd string) string {
	root := GitToplevel(cwd)
	switch {
	case root == "": // not a work tree: git's main-checkout probe would fail the same way
		root = resolveWith(cwd, func(string) string { return "" }).Origin
	case paths.SamePath(root, StoreDir()):
		root = Resolve(cwd).Origin
	}
	if root == "" {
		root, _ = filepath.Abs(cwd)
	}
	return root
}

// RecordSessionRoot preserves the first SessionStart root across resume and compaction.
func RecordSessionRoot(sessionID, cwd string) {
	if !LooksLikeSessionID(sessionID) || IsUpdateChild() || SessionRoot(sessionID, "") != "" {
		return
	}
	root := sessionRootFor(cwd)
	if root == "" {
		return
	}
	// Check and write under a per-session lock, rechecking inside it, so racing hooks keep one first root.
	path := sessionRootPath(sessionID)
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		var ran bool
		ran, err = paths.TryFileLock(path+".lock", sessionRootLockWait, func() error {
			if SessionRoot(sessionID, "") != "" {
				return nil
			}
			return paths.AtomicWrite(path, []byte(root+"\n"), 0o600)
		})
		if err == nil && !ran {
			err = fmt.Errorf("%w: %s.lock", paths.ErrLockTimeout, path)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "memory-start: record session root: %v\n", err)
	}
}

// SessionIDEnv is the env var Claude Code sets in every live session's tools.
const SessionIDEnv = "CLAUDE_CODE_SESSION_ID"

// sessionRoot is the transcript's recorded SessionStart root, else its project's cwd; "" if neither is known.
func sessionRoot(path string) string {
	if root := SessionRoot(strings.TrimSuffix(filepath.Base(path), ".jsonl"), ""); root != "" {
		return root
	}
	return projectCWD(path)
}

// projectCWD is the first cwd whose slug names the transcript's project dir; a session's first cwd may be elsewhere.
func projectCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	slug, found := filepath.Base(filepath.Dir(path)), ""
	_ = transcript.EachLine(f, func(line []byte) {
		var rec struct {
			CWD string `json:"cwd"`
		}
		if found == "" && json.Unmarshal(line, &rec) == nil && rec.CWD != "" && paths.ProjectSlug(rec.CWD) == slug {
			found = rec.CWD
		}
	})
	return found
}

// LiveTranscript resolves the running session's transcript: <id>.jsonl under projects/<cwd-slug>, else any project.
// It returns "" when sessionID is blank or no file exists, so a bare `memory update` outside a session stays a no-op.
func LiveTranscript(cwd, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	id := filepath.Base(sessionID)
	if !strings.HasSuffix(id, ".jsonl") {
		id += ".jsonl"
	}
	projects := filepath.Join(paths.ConfigDir(), "projects")
	if p := filepath.Join(projects, paths.ProjectSlug(cwd), id); paths.IsFile(p) {
		return p
	}
	if m, _ := filepath.Glob(filepath.Join(projects, "*", id)); len(m) > 0 {
		return m[0]
	}
	return ""
}
