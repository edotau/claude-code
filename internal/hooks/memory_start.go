package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/bank"
	memrecall "github.com/edotau/claude-code/internal/memory/recall"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
)

// defaultsDrift reads settings drift; var seam so tests can stub it.
var defaultsDrift = settings.DefaultsDrift

// sessionStart injects the memory bank (auto-scaffolding it for safe roots) as additionalContext.
func sessionStart(in *Input) Result {
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	memrecall.RecordSessionRoot(in.SessionID, cwd)
	ctx := memoryContext(cwd, in.SessionID, in.TranscriptPath, time.Now())
	res := Result{Context: joinContext(ctx, preflightNotice(time.Now()))}
	if !bank.IsUpdateChild() {
		res.SystemMessage = consumeCatchupNote()
	}
	return res
}

// preflightNotice is the static pin check (cached listings only): one short line when something needs a fix.
func preflightNotice(now time.Time) string {
	reg, err := providers.Load()
	if err != nil {
		return ""
	}
	return preflight.StaticLine(reg, now)
}

// memoryContext renders the note line + bank surface and advances the session chain.
func memoryContext(cwd, sessionID, transcriptPath string, now time.Time) string {
	prefix := maybeAutoInit(cwd) // a fresh-scaffold notice or ""
	if !bank.IsUpdateChild() {
		memstore.LinkNativeFor(cwd) // after the scaffold, so the link targets the store Init named (origin record in the vault)
	}
	chain := filepath.Join(memstore.StateDir(), "last-session")
	out := ""
	drift := driftNotice()
	if bank := memory.SessionSurface(cwd); bank != "" {
		note := bankLoadNote(prefix) + chainNote(chain, sessionID, now) + doctorNotice(cwd)
		out = strings.TrimSpace(note+"\n"+drift) + "\n\n" + bank
	} else {
		if prefix != "" {
			out = prefix // a fresh scaffold has no surface yet; still say where the bank went
		}
		out = strings.TrimSpace(out + "\n" + drift)
	}
	// The daily dream (consolidate) runs detached on Claude; never blocks launch.
	if !bank.IsUpdateChild() && memory.HasBank(cwd) && memory.DreamDue(cwd, now) {
		spawnDream(cwd)
	}
	// Every start sweeps for finished sessions whose update was missed, detached; the child does all the scanning.
	if !bank.IsUpdateChild() && memrecall.LooksLikeSessionID(sessionID) {
		spawnCatchup(cwd, sessionID)
	}
	// A stale served-endpoint cache is re-listed detached; needs no memory bank.
	if !bank.IsUpdateChild() && servedRefreshDue(now) {
		spawnServed(cwd)
	}
	// Every SessionStart (startup, resume, clear, compact) restarts what recall treats as already in context.
	if !bank.IsUpdateChild() {
		memrecall.ResetSeen(sessionID, cwd, out)
	}
	// An update worker is a session too; it must not become the "previous session" of the user's next one. Non-UUID ids (probe/test payloads) must not clobber the real previous-session pointer.
	if memrecall.LooksLikeSessionID(sessionID) && !bank.IsUpdateChild() {
		writeChain(chain, chainEntry{sessionID: sessionID, transcriptPath: transcriptPath, cwd: cwd})
	}
	return out
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
	entry, ok := readChain(chain)
	if !ok {
		return ""
	}
	prevID := entry.sessionID
	note := ""
	if prevID != sessionID {
		note = " Previous session id: " + prevID + " (reference for the prior session's transcript / claude --resume)."
	}
	if res, ok := memory.ReadUpdateResult(prevID); ok && res.NeedsAttention(now) {
		hint := ""
		if res.Log != "" {
			hint = " (log: " + res.Log + ")"
		}
		note += " NOTE: the previous session's memory update did not complete (" + res.Status + ")" + hint +
			" — run `claude-code memory update` to recover it."
	}
	return note
}

// doctorNotice is the cached memory-review notice line (from `memory update`'s settle path), or "" with none. A full Doctor pass tokenizes the home bank + access log, which has no place on the launch path.
func doctorNotice(root string) string {
	data, err := os.ReadFile(memory.DoctorNoticePath(root))
	if err != nil {
		return ""
	}
	notice := strings.TrimSpace(string(data))
	if notice == "" {
		return ""
	}
	if rs := []rune(notice); len(rs) > 200 {
		notice = string(rs[:200])
	}
	return " memory review: " + notice + "."
}

// driftNotice returns a one-line settings drift notice vs defaults, or "" when clean or on error.
func driftNotice() string {
	if bank.IsUpdateChild() {
		return ""
	}
	keys, err := defaultsDrift()
	if err != nil || len(keys) == 0 {
		return ""
	}
	if len(keys) == 1 {
		return "Settings: 1 key differs from your defaults — claude-code configure --restore"
	}
	return fmt.Sprintf("Settings: %d keys differ from your defaults — claude-code configure --restore", len(keys))
}

// chainEntry is the session chain file: id (line 1), transcript path (line 2), cwd (line 3) — lines 2-3 are optional so a pre-upgrade one-line file still parses.
type chainEntry struct {
	sessionID      string
	transcriptPath string
	cwd            string
}

func readChain(path string) (chainEntry, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return chainEntry{}, false
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	id := strings.TrimSpace(lines[0])
	if id == "" {
		return chainEntry{}, false
	}
	e := chainEntry{sessionID: id}
	if len(lines) > 1 {
		e.transcriptPath = strings.TrimSpace(lines[1])
	}
	if len(lines) > 2 {
		e.cwd = strings.TrimSpace(lines[2])
	}
	return e, true
}

func writeChain(path string, e chainEntry) {
	if err := paths.AtomicWrite(path, []byte(e.sessionID+"\n"+e.transcriptPath+"\n"+e.cwd+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "session-start: write chain %s: %v\n", path, err)
	}
}

// maybeAutoInit scaffolds a bank for bank-less safe roots; returns a notice on a fresh scaffold.
func maybeAutoInit(cwd string) string {
	if cwd == "" || memory.HasBank(cwd) || !scaffoldSafe(cwd) {
		return ""
	}
	dir, err := memory.Init(cwd)
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
	top := memstore.GitToplevel(cwd)
	return top != "" && paths.SamePath(top, cwd)
}

// servedRefreshDue reports whether served routing is on and an in-use Databricks profile has no fresh listing.
func servedRefreshDue(now time.Time) bool {
	if !providers.ServedRoutingOn() {
		return false
	}
	reg, err := providers.Load()
	return err == nil && preflight.Stale(reg, now) && preflight.Claim(now)
}

// spawnServed is the detached `preflight --refresh` seam; tests replace it.
var spawnServed = func(cwd string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	defer null.Close()
	return proc.SpawnDetached(self, []string{"preflight", "--refresh"}, cwd, null)
}

// spawnDream is the detached memory-dream seam; tests replace it.
var spawnDream = func(cwd string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	logPath := filepath.Join(memstore.StateDir(), "dream-"+time.Now().Format("20060102-150405")+".log")
	if os.MkdirAll(filepath.Dir(logPath), 0o700) != nil {
		return false
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	defer log.Close()
	return proc.SpawnDetached(self, []string{"memory", "dream", "--cwd", cwd}, cwd, log)
}
