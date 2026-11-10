package memhook

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// Start injects the memory bank (auto-scaffolding it for safe roots) as additionalContext.
func Start(in *core.Input) core.Result {
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	memory.RecordSessionRoot(in.SessionID, cwd)
	ctx := memoryContext(cwd, in.SessionID, time.Now())
	res := core.Result{Context: core.JoinContext(ctx, preflightNotice(time.Now()))}
	if !memory.IsUpdateChild() {
		res.SystemMessage = consumeCatchupNote()
	}
	if in.Source == "compact" || in.Source == "clear" {
		if note := resumeNote(in, cwd, time.Now()); note != "" {
			res.SystemMessage = joinMessages(res.SystemMessage, note)
			res.Context = core.JoinContext(res.Context, note)
		}
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
func memoryContext(cwd, sessionID string, now time.Time) string {
	prefix := maybeAutoInit(cwd) // a fresh-scaffold notice or ""
	if !memory.IsUpdateChild() {
		memory.LinkNativeFor(cwd) // after the scaffold, so the link targets the store Init named (origin record in the vault)
	}
	chain := filepath.Join(memory.StateDir(), "last-session")
	out := sessionSurfaceText(cwd, prefix, chain, sessionID, now)
	if !memory.IsUpdateChild() {
		spawnStartWorkers(cwd, sessionID, now)
		// Every SessionStart (startup, resume, clear, compact) restarts what recall treats as already in context.
		memory.ResetSeen(sessionID, cwd, out)
	}
	// An update worker is a session too; it must not become the "previous session" of the user's next one. Non-UUID ids (probe/test payloads) must not clobber the real previous-session pointer.
	if memory.LooksLikeSessionID(sessionID) && !memory.IsUpdateChild() {
		writeChain(chain, sessionID)
	}
	return out
}

// sessionSurfaceText renders the start-of-session note and (when a bank exists) its surface.
func sessionSurfaceText(cwd, prefix, chain, sessionID string, now time.Time) string {
	if surface := memory.SessionSurface(cwd); surface != "" {
		note := bankLoadNote(prefix) + chainNote(chain, sessionID, now) + memory.DoctorNotice(cwd)
		return strings.TrimSpace(note) + "\n\n" + surface
	}
	// a fresh scaffold has no surface yet; still say where the bank went
	return strings.TrimSpace(prefix)
}

// spawnStartWorkers launches the detached dream, catch-up sweep and served-endpoint refresh a start may owe.
func spawnStartWorkers(cwd, sessionID string, now time.Time) {
	// The daily dream (consolidate) runs detached on Claude; never blocks launch.
	if memory.HasBank(cwd) && memory.DreamDue(cwd, now) {
		spawnDream(cwd)
	}
	// Every start sweeps for finished sessions whose update was missed, detached; the child does all the scanning.
	if memory.LooksLikeSessionID(sessionID) {
		spawnCatchup(cwd, sessionID)
	}
	// A stale served-endpoint cache is re-listed detached; needs no memory bank.
	if servedRefreshDue(now) {
		spawnServed(cwd)
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
	top := memory.GitToplevel(cwd)
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
	logPath := filepath.Join(memory.StateDir(), "dream-"+time.Now().Format("20060102-150405")+".log")
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

const (
	logStampLayout = "20060102-150405"
	catchupWindow  = 14 * 24 * time.Hour // transcripts older than this are never swept
	catchupMax     = 3                   // sessions saved per sweep, newest first
	catchupScan    = 200                 // leading transcript lines searched for the entrypoint
)

// sweepCand is one finished session the sweep would save.
type sweepCand struct {
	id, path, why string
	edits         int
	mtime         time.Time
}

func catchupDonePath() string { return filepath.Join(memory.StateDir(), "catchup-done") }

// transcriptInteractive: a person's session has entrypoint "cli"; `claude -p` workers, leaves and pings say "sdk-cli".
func transcriptInteractive(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for range catchupScan {
		line, err := r.ReadBytes('\n')
		var rec struct {
			Entrypoint string `json:"entrypoint"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Entrypoint != "" {
			return rec.Entrypoint == "cli"
		}
		if err != nil {
			break
		}
	}
	return false
}

// readOnlyEdits is countEdits without cache writes, for --dry-run.
func readOnlyEdits(trPath, countFile string) int { return countWork(trPath, countFile, false) }

// sweepSkip is why a finished session needs no save ("" = it does): cheap checks before any transcript read.
func sweepSkip(c sweepCand, current string, now time.Time) string {
	switch {
	case c.id == current:
		return "current session"
	case now.Sub(c.mtime) < updateRearmCooldown:
		return "live (modified within cooldown)"
	case !transcriptInteractive(c.path):
		return "headless (worker/leaf/ping)"
	}
	if res, ok := memory.ReadUpdateResult(c.id); ok && res.Status == memory.UpdateOK && res.Time.After(c.mtime) {
		return "already saved"
	}
	return ""
}

// sweepWhy returns why an eligible session needs saving, or "" when its edits are covered.
func sweepWhy(c *sweepCand, now time.Time, count func(string, string) int) string {
	res, hasResult := memory.ReadUpdateResult(c.id)
	st, hasStamp := readStamp(stampPath(c.id))
	covered := 0
	if hasStamp {
		covered = st.edits
	}
	c.edits = count(c.path, countPath(c.id))
	if hasResult && res.NeedsAttention(now) && (!hasStamp || st.state != "catchup") {
		return "update " + res.Status
	}
	if c.edits >= updateMinEdits && c.edits > covered {
		return fmt.Sprintf("%d edits, %d covered", c.edits, covered)
	}
	return ""
}

// sweepCandidates lists finished sessions needing a save, newest first; all=false stops at catchupMax.
func sweepCandidates(current string, now time.Time, all bool, count func(string, string) int) ([]sweepCand, map[string]int) {
	recent := recentSessions(now)
	var out []sweepCand
	skips := map[string]int{}
	for _, c := range recent {
		if !all && len(out) >= catchupMax {
			break
		}
		skip := sweepSkip(c, current, now)
		if skip == "" {
			if c.why = sweepWhy(&c, now, count); c.why == "" {
				skip = "nothing new"
			}
		}
		if skip != "" {
			skips[skip]++
			continue
		}
		out = append(out, c)
	}
	return out, skips
}

// recentSessions lists transcripts modified inside the catch-up window, newest first.
func recentSessions(now time.Time) []sweepCand {
	files, _ := filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", "*.jsonl"))
	var recent []sweepCand
	for _, p := range files {
		id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		fi, err := os.Stat(p)
		if err != nil || !memory.LooksLikeSessionID(id) || now.Sub(fi.ModTime()) > catchupWindow {
			continue
		}
		recent = append(recent, sweepCand{id: id, path: p, mtime: fi.ModTime()})
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].mtime.After(recent[j].mtime) })
	return recent
}

// SweepOptions are `memory catchup`'s parsed flags: DryRun lists without saving, Skip is the session to leave alone.
type SweepOptions struct {
	DryRun         bool
	Skip           string
	Stdout, Stderr io.Writer
}

// CatchUpSweep is `memory catchup`: saves up to catchupMax finished sessions whose update was missed.
func CatchUpSweep(o SweepOptions) int {
	stdout, stderr := o.Stdout, o.Stderr
	current := o.Skip
	now := time.Now()
	if o.DryRun {
		cands, skips := sweepCandidates(current, now, true, readOnlyEdits)
		for i, c := range cands {
			verb := "would save"
			if i >= catchupMax {
				verb = "queued (cap)"
			}
			fmt.Fprintf(stdout, "%s %s  %s  (%s)\n", verb, c.id, c.mtime.Format("2006-01-02 15:04"), c.why)
		}
		fmt.Fprintf(stdout, "skipped: %v\n", skips)
		return 0
	}
	if err := os.MkdirAll(memory.StateDir(), 0o700); err != nil {
		fmt.Fprintf(stderr, "memory catchup: %v\n", err)
		return 1
	}
	_, err := paths.TryFileLock(filepath.Join(memory.StateDir(), "catchup.lock"), 0, func() error {
		runSweep(current, now, stdout)
		return nil
	})
	if err != nil {
		fmt.Fprintf(stderr, "memory catchup: %v\n", err)
		return 1
	}
	return 0 // a busy lock means another sweep is running
}

// runSweep saves each candidate in turn and records how many ended ok for the next SessionStart.
func runSweep(current string, now time.Time, out io.Writer) {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(out, "memory catchup:", err)
		return
	}
	cands, _ := sweepCandidates(current, now, false, countEdits)
	saved := 0
	for _, c := range cands {
		// Stamped first, so a crash or failure retries at most once.
		if err := writeStamp(stampPath(c.id), time.Now(), "catchup", c.edits); err != nil {
			fmt.Fprintf(out, "catchup %s: write retry stamp: %v\n", c.id, err)
			continue
		}
		fmt.Fprintf(out, "catchup %s (%s)\n", c.id, c.why)
		ctx, cancel := context.WithTimeout(context.Background(), memory.UpdateTimeout+2*time.Minute)
		started := time.Now()
		if err := runHistoricalUpdate(ctx, self, c, out); err != nil {
			fmt.Fprintf(out, "catchup %s: %v\n", c.id, err)
		}
		cancel()
		// An older OK receipt from a previous update must not count as this run's save.
		if res, ok := memory.ReadUpdateResult(c.id); ok && res.Status == memory.UpdateOK && res.Time.After(started) {
			saved++
		}
	}
	if saved > 0 {
		prev, _ := strconv.Atoi(strings.TrimSpace(readFileOr(catchupDonePath())))
		_ = paths.AtomicWrite(catchupDonePath(), []byte(strconv.Itoa(prev+saved)), 0o600)
	}
}

// runHistoricalUpdate is the subprocess boundary; tests never execute their own binary.
var runHistoricalUpdate = func(ctx context.Context, self string, c sweepCand, out io.Writer) error {
	cmd := exec.CommandContext(ctx, self, "memory", "update", "--historical", "--session", c.id, "--transcript", c.path)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

func readFileOr(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// consumeCatchupNote is the one quiet line for a finished sweep (read once), or "" with nothing saved.
func consumeCatchupNote() string {
	if _, err := os.Stat(catchupDonePath()); err != nil {
		return "" // common case: no note, so skip the lock
	}
	n := 0
	// A running sweep holds the lock and still updates the count: leave it for the next start.
	_, _ = paths.TryFileLock(filepath.Join(memory.StateDir(), "catchup.lock"), 0, func() error {
		n, _ = strconv.Atoi(strings.TrimSpace(readFileOr(catchupDonePath())))
		return os.Remove(catchupDonePath())
	})
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "memory: saved 1 earlier session"
	}
	return fmt.Sprintf("memory: saved %d earlier sessions", n)
}

// spawnCatchup is the detached sweep seam; tests replace it.
var spawnCatchup = func(cwd, sessionID string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	logPath := filepath.Join(memory.StateDir(), "catchup-"+time.Now().Format(logStampLayout)+".log")
	if os.MkdirAll(filepath.Dir(logPath), 0o700) != nil {
		return false
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	defer log.Close()
	return proc.SpawnDetached(self, []string{"memory", "catchup", "--skip", sessionID}, cwd, log)
}
