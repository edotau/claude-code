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
	"github.com/edotau/claude-code/internal/transcript"
	mdast "github.com/jeduden/mdsmith/pkg/goldmark/ast"
	"github.com/jeduden/mdsmith/pkg/goldmark/parser"
	"github.com/jeduden/mdsmith/pkg/markdown"
)

// hasBank is bankExists behind a seam; tests count the bank probes one start makes.
var hasBank = bankExists

// StartSession records the session root, renders the note line + bank surface and advances the session chain.
func StartSession(sessionID, cwd string, now time.Time) string {
	recordSessionRoot(sessionID, cwd)
	prefix, banked := maybeAutoInit(cwd, hasBank(cwd)) // the gate probe; a scaffold and the surface re-probe
	if !IsUpdateChild() {
		linkNativeFor(cwd) // after the scaffold, so the link targets the store Init named (origin record in the vault)
	}
	chain := filepath.Join(StateDir(), "last-session")
	out := sessionSurfaceText(cwd, prefix, chain, sessionID, now)
	if !IsUpdateChild() {
		startWorkers(cwd, sessionID, now, banked)
		// Every SessionStart (startup, resume, clear, compact) restarts what recall treats as already in context.
		resetSeen(sessionID, cwd, out)
	}
	// An update worker is never the next "previous session"; a non-UUID (probe) id must not clobber the pointer.
	if looksLikeSessionID(sessionID) && !IsUpdateChild() {
		writeChain(chain, sessionID)
	}
	return out
}

// SessionText is StartSession's text for an audit: no scaffold, link, workers, seen reset or chain write.
func SessionText(sessionID, cwd string, now time.Time) string {
	return sessionSurfaceText(cwd, "", filepath.Join(StateDir(), "last-session"), sessionID, now)
}

// sessionSurfaceText renders the start-of-session note and (when a bank exists) its surface.
func sessionSurfaceText(cwd, prefix, chain, sessionID string, now time.Time) string {
	if surface := sessionSurface(cwd); surface != "" {
		note := bankLoadNote(prefix) + chainNote(chain, sessionID, now) + doctorNotice(cwd)
		return strings.TrimSpace(note) + "\n\n" + surface
	}
	// a fresh scaffold has no surface yet; still say where the bank went
	return strings.TrimSpace(prefix)
}

// startWorkers launches the detached dream and catch-up sweep a start may owe; banked is cwd's bank probe.
func startWorkers(cwd, sessionID string, now time.Time, banked bool) {
	// The daily dream (consolidate) runs detached on Claude; never blocks launch.
	if banked && dreamDue(cwd, now) {
		spawn.dream(cwd)
	}
	// Every start sweeps for finished sessions whose update was missed, detached; the child does all the scanning.
	if looksLikeSessionID(sessionID) {
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
	if res, ok := readUpdateResult(prevID); ok && res.needsAttention(now) {
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

// maybeAutoInit scaffolds a bank for bank-less safe roots; returns a notice on a fresh scaffold and cwd's bank probe.
func maybeAutoInit(cwd string, banked bool) (string, bool) {
	if cwd == "" || banked || !scaffoldSafe(cwd) {
		return "", banked
	}
	dir, err := Init(cwd)
	banked = hasBank(cwd) // a scaffold, even a failed partial one, changes the answer
	if err != nil {
		return "", banked
	}
	return "Memory bank scaffolded at " + dir + " — it fills automatically at session end (optionally enrich with `claude-code memory init`).", banked
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

func prunableSessionState(name string) bool {
	for _, prefix := range []string{"count-", "stamp-", "watch-"} {
		if strings.HasPrefix(name, prefix) && !strings.HasSuffix(name, ".lock") { // a live session's lock looks old
			return true
		}
	}
	for _, prefix := range []string{"update-", "dream-", "catchup-"} {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".log") {
			return true
		}
	}
	return false
}

// staleSessionRoot reports a root-<id> or <prefix>-<id>.lock file whose session has no newer count/stamp/watch file.
func staleSessionRoot(dir, name string, cutoff time.Time) bool {
	base, isLock := strings.CutSuffix(name, ".lock")
	id, ok := strings.CutPrefix(base, "root-")
	for _, prefix := range []string{"count-", "stamp-", "watch-"} {
		if rest, found := strings.CutPrefix(base, prefix); found && isLock {
			id, ok = rest, true
		}
	}
	if !ok {
		return false
	}
	for _, prefix := range []string{"count-", "stamp-", "watch-"} {
		if fi, err := os.Stat(filepath.Join(dir, prefix+id)); err == nil && !fi.ModTime().Before(cutoff) {
			return false
		}
	}
	return true
}

// sessionRootLockWait bounds how long a hook waits for a racing repair to finish.
var sessionRootLockWait = 5 * time.Second

func sessionRootPath(sessionID string) string {
	return filepath.Join(StateDir(), "root-"+filepath.Base(sessionID))
}

// SessionRoot uses the launch repository when available, otherwise the hook payload cwd.
func SessionRoot(sessionID, cwd string) string {
	if !looksLikeSessionID(sessionID) || IsUpdateChild() {
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

// recordSessionRoot preserves the first SessionStart root across resume and compaction.
func recordSessionRoot(sessionID, cwd string) {
	if !looksLikeSessionID(sessionID) || IsUpdateChild() || SessionRoot(sessionID, "") != "" {
		return
	}
	root := sessionRootFor(cwd)
	if root == "" {
		return
	}
	if err := writeSessionRoot(sessionID, root); err != nil {
		fmt.Fprintf(os.Stderr, "memory-start: record session root: %v\n", err)
	}
}

// writeSessionRoot writes root under a per-session lock, rechecking inside it, so racing hooks keep one first root.
func writeSessionRoot(sessionID, root string) error {
	path := sessionRootPath(sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	ran, err := paths.TryFileLock(path+".lock", sessionRootLockWait, func() error {
		if SessionRoot(sessionID, "") != "" {
			return nil
		}
		return paths.AtomicWrite(path, []byte(root+"\n"), 0o600)
	})
	if err == nil && !ran {
		err = fmt.Errorf("%w: %s.lock", paths.ErrLockTimeout, path)
	}
	return err
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

// LiveTranscript is <id>.jsonl under the cwd's project, else any project; "" for a blank id or no file (a no-op).
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
	if m := globIn(projects, "*/"+globLiteral(id)); len(m) > 0 {
		return m[0]
	}
	return ""
}

// Surface caps: a budget under hooks' 9,500-char additionalContext cap (~1,000 left for the note).
const (
	totalCapChars = 8500
	indexCapChars = 2500
)

// Session-surface budgets: this repo's focus and the home bank's essentials, reserved so neither starves the other.
const (
	focusCapChars     = 2000
	homeFocusCapChars = 800
)

// sessionSurface is the capped session preload: root's and home's focus sections plus a pointer to other repos.
func sessionSurface(root string) string {
	dir := Dir(root)
	var parts []string
	if _, err := os.Stat(filepath.Join(StoreDir(), catalogFile)); err == nil {
		parts = append(parts, "<!-- other repos: `claude-code memory search --all <terms>`; catalog "+filepath.Join(StoreDir(), catalogFile)+" -->")
	}
	if bankExists(root) { // the header alone still names the bank when there is no focus text yet
		head := "<!-- THIS REPO (" + dir + ") — focus only; read the typed files on demand -->"
		if focus := repoFocus(dir); len(focus) > 0 {
			head += "\n" + strings.Join(focus, "\n\n")
		}
		parts = append(parts, capChars(head, focusCapChars))
	}
	if home := HomeBankDir(); !isHomeBank(root) {
		if focus := repoFocus(home); len(focus) > 0 {
			parts = append(parts, capChars("<!-- HOME BANK ("+home+") — essentials -->\n"+strings.Join(focus, "\n\n"), homeFocusCapChars))
		}
	}
	if !nativeLinked(root, dir) { // Claude Code is reading another dir: carry the store's index ourselves
		if data, err := os.ReadFile(filepath.Join(dir, nativeIndexFile)); err == nil && len(data) > 0 {
			parts = append(parts, capChars("<!-- THIS REPO's MEMORY.md (native link missing or blocked) -->\n"+strings.TrimSpace(string(data)), indexCapChars))
		}
	}
	return capChars(strings.Join(parts, "\n\n"), totalCapChars)
}

// repoFocus is a bank dir's injected sections: activeContext focus/blockers/next steps and progress "In Progress".
func repoFocus(dir string) []string {
	var focus []string
	if data, err := os.ReadFile(filepath.Join(dir, "activeContext.md")); err == nil {
		heads := []string{"Current Focus", "Blockers", "Next Steps"}
		for i, sec := range sectionBodies(string(data), heads...) {
			if sec != "" {
				focus = append(focus, "## "+heads[i]+"\n"+sec)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "progress.md")); err == nil {
		if sec := sectionBody(string(data), "In Progress"); sec != "" {
			focus = append(focus, "## In Progress\n"+sec)
		}
	}
	return focus
}

// nativeLinked reports whether Claude Code's native memory dir for root is the symlink into dir.
func nativeLinked(root, dir string) bool {
	r := filepath.Clean(root)
	if top := gitMain(r); top != "" {
		r = paths.CanonDeep(top)
	}
	target := nativeLinkTarget(projectsMemory(paths.ProjectSlug(r)))
	return target != "" && paths.SamePath(target, dir)
}

// sectionBody is the text under "## heading" up to the next "## " heading; "" when absent or empty.
func sectionBody(text, heading string) string { return sectionBodies(text, heading)[0] }

// sectionBodies is sectionBody per heading from one AST parse, so a "## " line inside a fence never ends a section.
func sectionBodies(text string, headings ...string) []string {
	doc := markdown.Parse([]byte(text))
	heads := level2Headings(doc.Body)
	out := make([]string, len(headings))
	for n, heading := range headings {
		for i, h := range heads {
			if !strings.EqualFold(h.title, heading) {
				continue
			}
			end := len(doc.Body)
			if i+1 < len(heads) {
				end = heads[i+1].lineStart
			}
			out[n] = strings.TrimSpace(string(doc.Body[h.lineEnd:end]))
			break
		}
	}
	return out
}

// headingSpan is a level-2 ("## ") heading's title and the byte range of its own line, body-relative.
type headingSpan struct {
	title              string
	lineStart, lineEnd int
}

// level2Headings returns every "## " heading's title and line span; body is already front-matter-free.
func level2Headings(body []byte) []headingSpan {
	var out []headingSpan
	root := markdown.ParseContext(body, parser.NewContext())
	_ = mdast.Walk(root, func(n mdast.Node, entering bool) (mdast.WalkStatus, error) { // walker never errors
		if !entering {
			return mdast.WalkContinue, nil
		}
		hn, ok := n.(*mdast.Heading)
		if !ok || hn.Level != 2 {
			return mdast.WalkContinue, nil
		}
		if hn.Lines().Len() == 0 { // a bare "## " has no title and no segment to locate it by
			return mdast.WalkContinue, nil
		}
		out = append(out, headingLineSpan(body, hn))
		return mdast.WalkContinue, nil
	})
	return out
}

func headingLineSpan(body []byte, hn *mdast.Heading) headingSpan {
	seg := hn.Lines().At(0)
	lineStart := seg.Start
	for lineStart > 0 && body[lineStart-1] != '\n' {
		lineStart--
	}
	lineEnd := seg.Stop
	for lineEnd < len(body) && body[lineEnd] != '\n' {
		lineEnd++
	}
	if lineEnd < len(body) {
		lineEnd++
	}
	return headingSpan{title: string(seg.Value(body)), lineStart: lineStart, lineEnd: lineEnd}
}

const truncatedNotice = "\n<!-- (surface truncated) -->"

// capChars cuts s to at most max runes (marker included) at the last whole line.
func capChars(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	room := max - len([]rune(truncatedNotice))
	if room <= 0 {
		return ""
	}
	cut := string(r[:room])
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + truncatedNotice
}
