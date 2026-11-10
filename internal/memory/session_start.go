package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// StartSession records the session root, renders the note line + bank surface and advances the session chain.
func StartSession(sessionID, cwd string, now time.Time) string {
	RecordSessionRoot(sessionID, cwd)
	prefix := maybeAutoInit(cwd) // a fresh-scaffold notice or ""
	if !IsUpdateChild() {
		LinkNativeFor(cwd) // after the scaffold, so the link targets the store Init named (origin record in the vault)
	}
	chain := filepath.Join(StateDir(), "last-session")
	out := sessionSurfaceText(cwd, prefix, chain, sessionID, now)
	if !IsUpdateChild() {
		startWorkers(cwd, sessionID, now)
		// Every SessionStart (startup, resume, clear, compact) restarts what recall treats as already in context.
		ResetSeen(sessionID, cwd, out)
	}
	// An update worker is never the next "previous session"; a non-UUID (probe) id must not clobber the pointer.
	if LooksLikeSessionID(sessionID) && !IsUpdateChild() {
		writeChain(chain, sessionID)
	}
	return out
}

// sessionSurfaceText renders the start-of-session note and (when a bank exists) its surface.
func sessionSurfaceText(cwd, prefix, chain, sessionID string, now time.Time) string {
	if surface := SessionSurface(cwd); surface != "" {
		note := bankLoadNote(prefix) + chainNote(chain, sessionID, now) + doctorNotice(cwd)
		return strings.TrimSpace(note) + "\n\n" + surface
	}
	// a fresh scaffold has no surface yet; still say where the bank went
	return strings.TrimSpace(prefix)
}

// startWorkers launches the detached dream and catch-up sweep a start may owe.
func startWorkers(cwd, sessionID string, now time.Time) {
	// The daily dream (consolidate) runs detached on Claude; never blocks launch.
	if HasBank(cwd) && DreamDue(cwd, now) {
		spawn.dream(cwd)
	}
	// Every start sweeps for finished sessions whose update was missed, detached; the child does all the scanning.
	if LooksLikeSessionID(sessionID) {
		spawn.catchup(cwd, sessionID)
	}
}

// bankLoadNote is the fixed "session memory bank loaded" line, with an optional scaffold/pause prefix.
func bankLoadNote(prefix string) string {
	note := "Session memory bank (local + shared home) loaded below. It auto-saves at session end; `claude-code memory update` is the manual override."
	if prefix != "" {
		note = prefix + " " + note
	}
	return note
}

// chainNote appends the previous-session pointer and any catch-up/attention note, or "" with no chain entry.
func chainNote(chain, sessionID string, now time.Time) string {
	prevID, ok := readChain(chain)
	if !ok {
		return ""
	}
	note := ""
	if prevID != sessionID {
		note = " Previous session id: " + prevID + " (reference for the prior session's transcript / claude --resume)."
	}
	if res, ok := ReadUpdateResult(prevID); ok && res.NeedsAttention(now) {
		hint := ""
		if res.Log != "" {
			hint = " (log: " + res.Log + ")"
		}
		note += " NOTE: the previous session's memory update did not complete (" + res.Status + ")" + hint +
			" — run `claude-code memory update` to recover it."
	}
	return note
}

// readChain reads the previous session id from the first line.
func readChain(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	id, _, _ := strings.Cut(string(b), "\n")
	id = strings.TrimSpace(id)
	return id, id != ""
}

func writeChain(path, sessionID string) {
	if err := paths.AtomicWrite(path, []byte(sessionID+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "session-start: write chain %s: %v\n", path, err)
	}
}

// maybeAutoInit scaffolds a bank for bank-less safe roots; returns a notice on a fresh scaffold.
func maybeAutoInit(cwd string) string {
	if cwd == "" || HasBank(cwd) || !scaffoldSafe(cwd) {
		return ""
	}
	dir, err := Init(cwd)
	if err != nil {
		return ""
	}
	return "Memory bank scaffolded at " + dir + " — it fills automatically at session end (optionally enrich with `claude-code memory init`)."
}

// scaffoldSafe: the config dir, a dir already carrying .claude/, or a git TOPLEVEL (a subdir would misplace the bank).
func scaffoldSafe(cwd string) bool {
	if paths.SamePath(cwd, paths.ConfigDir()) {
		return true
	}
	if fi, err := os.Stat(filepath.Join(cwd, ".claude")); err == nil && fi.IsDir() {
		return true
	}
	top := GitToplevel(cwd)
	return top != "" && paths.SamePath(top, cwd)
}

// compactRecord is what PreCompact leaves for the next SessionStart(compact).
type compactRecord struct {
	Trigger string `json:"trigger"`
	Pct     int    `json:"pct"`
	At      string `json:"at"`
}

func compactRecordPath(sessionID string) string {
	return filepath.Join(paths.StateDir(), "compact", filepath.Base(sessionID)+".json")
}

// WriteCompactRecord notes the compaction for the next start; errors go to stderr only.
func WriteCompactRecord(sessionID, trigger string, pct int, now time.Time) {
	b, err := json.Marshal(compactRecord{Trigger: trigger, Pct: pct, At: now.Format(time.RFC3339)})
	if err == nil {
		path := compactRecordPath(sessionID)
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			err = paths.AtomicWrite(path, b, 0o600)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "context-steward: compact record: %v\n", err)
	}
}

// takeCompactRecord reads and deletes the session's compact record; ok is false when none was left.
func takeCompactRecord(sessionID string) (r compactRecord, ok bool) {
	path := compactRecordPath(sessionID)
	b, err := os.ReadFile(path)
	if err != nil {
		return r, false
	}
	_ = os.Remove(path)
	return r, json.Unmarshal(b, &r) == nil
}

// newestHandoff is the most recently modified handoff in dir within maxAge, or "".
func newestHandoff(dir string, now time.Time, maxAge time.Duration) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestMod := "", time.Time{}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || e.IsDir() || filepath.Ext(e.Name()) != ".md" || now.Sub(fi.ModTime()) > maxAge {
			continue
		}
		if fi.ModTime().After(bestMod) {
			best, bestMod = filepath.Join(dir, e.Name()), fi.ModTime()
		}
	}
	return best
}

// ResumeNote is the compact/clear start line: the compaction record and the fresh handoff, whichever exist.
func ResumeNote(source, sessionID, cwd string, now time.Time) string {
	var parts []string
	if source == "compact" {
		if r, ok := takeCompactRecord(sessionID); ok {
			parts = append(parts, compactedText(r))
		}
	}
	if h := newestHandoff(HandoffsDir(Dir(cwd)), now, 24*time.Hour); h != "" {
		parts = append(parts, "handoff: "+h)
	}
	return strings.Join(parts, "; ")
}

// compactedText renders "compacted (auto, 91%) at 14:02", dropping what the record lacks.
func compactedText(r compactRecord) string {
	var detail []string
	if r.Trigger != "" {
		detail = append(detail, r.Trigger)
	}
	if r.Pct > 0 {
		detail = append(detail, strconv.Itoa(r.Pct)+"%")
	}
	text := "compacted"
	if len(detail) > 0 {
		text += " (" + strings.Join(detail, ", ") + ")"
	}
	if at, err := time.Parse(time.RFC3339, r.At); err == nil {
		text += " at " + at.Local().Format("15:04")
	}
	return text
}
