package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

const (
	catchupWindow = 14 * 24 * time.Hour // transcripts older than this are never swept
	catchupMax    = 3                   // sessions saved per sweep, newest first
)

// sweepCand is one finished session the sweep would save.
type sweepCand struct {
	id, path, why string
	edits         int
	mtime         time.Time
}

func catchupDonePath() string { return filepath.Join(StateDir(), "catchup-done") }

// transcriptInteractive scans through the first user prompt, even after a long metadata prelude.
func transcriptInteractive(path string) (interactive bool) {
	interactive = true // Replaying a worker costs a call; skipping an uncertain session loses its save.
	entrypoint := ""
	scanTranscriptHead(path, math.MaxInt, func(line []byte) bool {
		var rec struct {
			Type       string `json:"type"`
			Entrypoint string `json:"entrypoint"`
			Message    struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil {
			return false
		}

		if rec.Entrypoint != "" {
			entrypoint = rec.Entrypoint
		}
		if rec.Type != "user" { // read on to the first prompt: a memory job inherits its parent's entrypoint
			return false
		}
		interactive = !strings.HasPrefix(entrypoint, "sdk-") && !memoryJobPrompt(transcriptPrompt(rec.Message.Content))
		return true
	})
	return interactive
}

// Worker prompt openings are followed by the bank-specific continuation below.
const (
	updatePromptOpen = "Background update of a finished session."
	dreamPromptOpen  = "Background daily consolidation of the memory bank."
)

// Prompt formats are shared with the builders so classification requires the worker-only continuation.
const (
	updatePromptFormat = updatePromptOpen + " The bank directory is %s and the shared home bank is %s; write only there. "
	dreamPromptFormat  = dreamPromptOpen + " The local bank is %s; the memory root %s holds the home bank and every repo store. "
)

var memoryJobPatterns = []*regexp.Regexp{workerPromptPattern(updatePromptFormat), workerPromptPattern(dreamPromptFormat)}

func workerPromptPattern(format string) *regexp.Regexp {
	return regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(format), "%s", `[^\r\n]+?`))
}

func memoryJobPrompt(s string) bool {
	return slices.ContainsFunc(memoryJobPatterns, func(re *regexp.Regexp) bool { return re.MatchString(s) })
}

func transcriptPrompt(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var out strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			out.WriteString(b.Text)
		}
	}
	return out.String()
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
	if hasResult && res.needsAttention(now) && (!hasStamp || st.state != "catchup") {
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
		if err != nil || !looksLikeSessionID(id) || now.Sub(fi.ModTime()) > catchupWindow {
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

// blockedNotePath holds the one quiet line a detached update leaves for the next SessionStart to show.
func blockedNotePath() string { return filepath.Join(StateDir(), "update-blocked-note") }

// noteOut is where a foreground update's quiet note goes; a var for tests.
var noteOut io.Writer = os.Stderr

// noteBlocked tells the user quietly: dim on stderr for a person's run, a persisted line for a detached worker.
func noteBlocked(log string, blocks int) {
	line := fmt.Sprintf("memory: this session's update was not saved (the permission gate refused %d write(s))", blocks)
	if log != "" {
		line += "; details: " + log
	}
	if v := os.Getenv(DetachedEnv); v != "" && v != "0" {
		_ = paths.AtomicWrite(blockedNotePath(), []byte(line+"\n"), 0o600)
		return
	}
	fmt.Fprintln(noteOut, ansi.Style(ansi.Enabled(noteOut), ansi.Dim, line))
}

// ConsumeCatchupNote is the quiet SessionStart note (read once): a finished sweep's saved count, a blocked update.
func ConsumeCatchupNote() string {
	return cmp.Or(strings.Join(slices.DeleteFunc([]string{consumeSavedNote(), consumeBlockedNote()},
		func(s string) bool { return s == "" }), "; "))
}

// consumeBlockedNote returns and deletes the line a detached update left when the gate refused its writes.
func consumeBlockedNote() string {
	b, err := os.ReadFile(blockedNotePath())
	if err != nil {
		return ""
	}
	_ = os.Remove(blockedNotePath())
	return strings.TrimSpace(string(b))
}

// consumeSavedNote is the one quiet line for a finished sweep (read once), or "" with nothing saved.
func consumeSavedNote() string {
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
