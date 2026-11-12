package memory

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

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

const (
	catchupWindow = 14 * 24 * time.Hour // transcripts older than this are never swept
	catchupMax    = 3                   // sessions saved per sweep, newest first
	catchupScan   = 200                 // leading transcript lines searched for the entrypoint
)

// sweepCand is one finished session the sweep would save.
type sweepCand struct {
	id, path, why string
	edits         int
	mtime         time.Time
}

func catchupDonePath() string { return filepath.Join(StateDir(), "catchup-done") }

// transcriptInteractive: `claude -p` workers, leaves and pings say "sdk-*"; any other entrypoint is a person.
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
			return !strings.HasPrefix(rec.Entrypoint, "sdk-")
		}
		if err != nil {
			break
		}
	}
	return false
}

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
	if res, ok := readUpdateResult(c.id); ok && res.Status == updateOK && res.Time.After(c.mtime) {
		return "already saved"
	}
	return ""
}

// sweepWhy returns why an eligible session needs saving, or "" when its edits are covered.
func sweepWhy(c *sweepCand, now time.Time, count func(string, string) int) string {
	res, hasResult := readUpdateResult(c.id)
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
	recent := recentTranscripts(now)
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

// recentTranscripts lists transcripts modified inside the catch-up window, newest first.
func recentTranscripts(now time.Time) []sweepCand {
	var recent []sweepCand
	for _, p := range globIn(paths.ConfigDir(), "projects/*/*.jsonl") {
		id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		fi, err := os.Stat(p)
		if err != nil || !LooksLikeSessionID(id) || now.Sub(fi.ModTime()) > catchupWindow {
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
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		fmt.Fprintf(stderr, "memory catchup: %v\n", err)
		return 1
	}
	_, err := paths.TryFileLock(filepath.Join(StateDir(), "catchup.lock"), 0, func() error {
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
		ctx, cancel := context.WithTimeout(context.Background(), updateTimeout+2*time.Minute)
		started := time.Now()
		if err := runHistoricalUpdate(ctx, self, c, out); err != nil {
			fmt.Fprintf(out, "catchup %s: %v\n", c.id, err)
		}
		cancel()
		// An older OK receipt from a previous update must not count as this run's save.
		if res, ok := readUpdateResult(c.id); ok && res.Status == updateOK && res.Time.After(started) {
			saved++
		}
	}
	if saved > 0 {
		prev := catchupDoneCount()
		if err := paths.AtomicWrite(catchupDonePath(), []byte(strconv.Itoa(prev+saved)), 0o600); err != nil {
			fmt.Fprintf(out, "memory catchup: %d session(s) saved but the done note failed: %v\n", saved, err)
		}
	}
}

// runHistoricalUpdate is the subprocess boundary; tests never execute their own binary.
var runHistoricalUpdate = func(ctx context.Context, self string, c sweepCand, out io.Writer) error {
	cmd := exec.CommandContext(ctx, self, "memory", "update", "--historical", "--session", c.id, "--transcript", c.path)
	cmd.Stdout, cmd.Stderr = out, out
	proc.OwnGroup(cmd) // the timeout kills the update's whole group, so its claude child cannot outlive the lock
	cmd.WaitDelay = proc.ExecWaitDelay
	return cmd.Run()
}

// catchupDoneCount is the saved-session count in the done note; 0 when absent or unreadable.
func catchupDoneCount() int {
	b, _ := readIfExists(catchupDonePath())
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// ConsumeCatchupNote is the one quiet line for a finished sweep (read once), or "" with nothing saved.
func ConsumeCatchupNote() string {
	if _, err := os.Stat(catchupDonePath()); err != nil {
		return "" // common case: no note, so skip the lock
	}
	n := 0
	// A running sweep holds the lock and still updates the count: leave it for the next start.
	_, _ = paths.TryFileLock(filepath.Join(StateDir(), "catchup.lock"), 0, func() error {
		n = catchupDoneCount()
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
