package hooks

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

	"github.com/edotau/claude-code/internal/memory"
	memrecall "github.com/edotau/claude-code/internal/memory/recall"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

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

func catchupDonePath() string { return filepath.Join(memstore.StateDir(), "catchup-done") }

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
	files, _ := filepath.Glob(filepath.Join(paths.ConfigDir(), "projects", "*", "*.jsonl"))
	var recent []sweepCand
	skips := map[string]int{}
	for _, p := range files {
		id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		fi, err := os.Stat(p)
		if err != nil || !memrecall.LooksLikeSessionID(id) || now.Sub(fi.ModTime()) > catchupWindow {
			continue
		}
		recent = append(recent, sweepCand{id: id, path: p, mtime: fi.ModTime()})
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].mtime.After(recent[j].mtime) })
	var out []sweepCand
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

// CatchUpSweep is `memory catchup`: saves up to catchupMax finished sessions whose update was missed.
func CatchUpSweep(args []string, stdout, stderr io.Writer) int {
	fs := helptext.NewFlagSet("memory catchup", helptext.Page{Usage: "claude-code memory catchup [flags]",
		About: "Session-start sweep: saves finished sessions whose memory update was missed."})
	fs.SetOutput(stderr)
	dry := fs.Bool("dry-run", false, "list the sessions it would save; write nothing")
	skip := fs.String("skip", "", "session id to leave alone (the one that started the sweep)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	current := *skip
	now := time.Now()
	if *dry {
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
	if err := os.MkdirAll(memstore.StateDir(), 0o700); err != nil {
		fmt.Fprintf(stderr, "memory catchup: %v\n", err)
		return 1
	}
	_, err := paths.TryFileLock(filepath.Join(memstore.StateDir(), "catchup.lock"), 0, func() error {
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
		_ = writeStamp(stampPath(c.id), time.Now(), "catchup", c.edits)
		fmt.Fprintf(out, "catchup %s (%s)\n", c.id, c.why)
		ctx, cancel := context.WithTimeout(context.Background(), memory.UpdateTimeout+2*time.Minute)
		cmd := exec.CommandContext(ctx, self, "memory", "update", "--historical", "--session", c.id, "--transcript", c.path)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(out, "catchup %s: %v\n", c.id, err)
		}
		cancel()
		if res, ok := memory.ReadUpdateResult(c.id); ok && res.Status == memory.UpdateOK {
			saved++
		}
	}
	if saved > 0 {
		prev, _ := strconv.Atoi(strings.TrimSpace(readFileOr(catchupDonePath())))
		_ = paths.AtomicWrite(catchupDonePath(), []byte(strconv.Itoa(prev+saved)), 0o600)
	}
}

func readFileOr(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// consumeCatchupNote is the one quiet line for a finished sweep (read once), or "" with nothing saved.
func consumeCatchupNote() string {
	n, _ := strconv.Atoi(strings.TrimSpace(readFileOr(catchupDonePath())))
	_ = os.Remove(catchupDonePath())
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
	logPath := filepath.Join(memstore.StateDir(), "catchup-"+time.Now().Format(logStampLayout)+".log")
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
