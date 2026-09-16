package hooks

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/transcript"
)

// Harvest gate thresholds: the floor asks "was this substantive work"; re-arming needs 10 NEW edits AND 15 min,
// since every harvest appends a sessionHistory entry and a chatty re-arm evicts other sessions' history.
const (
	harvestMinEdits   = 3
	harvestRearmEdits = 10
	reharvestCooldown = 15 * time.Minute
	harvestMaxBlocks  = 3
)

// SessionStart injects the memory bank (auto-scaffolding it for safe roots) as additionalContext.
func SessionStart(r io.Reader, stdout io.Writer) int {
	in := ParseInput(r)
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if out := MergeContext("SessionStart", []string{memoryContext(cwd, in.SessionID, time.Now())}); out != "" {
		fmt.Fprintln(stdout, out)
	}
	return ExitProceed
}

// memoryContext renders the note line + bank surface and advances the session chain.
func memoryContext(cwd, sessionID string, now time.Time) string {
	scaffolded := maybeAutoInit(cwd)
	chain := filepath.Join(memory.StateDir(), "last-session")
	out := ""
	if bank := memory.SessionSurface(cwd); bank != "" {
		note := "Session memory bank (local + shared global) loaded below. It auto-saves at session end; /memory:end is the manual override."
		if scaffolded != "" {
			note = scaffolded + " " + note
		}
		if prev, _ := os.ReadFile(chain); strings.TrimSpace(string(prev)) != "" {
			prevID := strings.TrimSpace(string(prev))
			note += " Previous session id: " + prevID + " (reference for the prior session's transcript / claude --resume)."
			if res, ok := memory.ReadHarvestResult(prevID); ok && res.NeedsAttention(now) {
				hint := ""
				if res.Log != "" {
					hint = " (log: " + res.Log + ")"
				}
				note += " NOTE: the previous session's memory harvest did not complete (" + res.Status + ")" + hint + " — run /memory:end to recover it."
			}
		}
		out = note + "\n\n" + bank
	}
	// A harvest worker is a session too; it must not become the "previous session" of the user's next one.
	if sessionID != "" && !memory.IsHarvestChild() {
		_ = paths.AtomicWrite(chain, []byte(sessionID+"\n"), 0o600)
	}
	return out
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
	return "Memory bank scaffolded at " + dir + " — it fills automatically at session end (optionally enrich with /memory:init)."
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

// SessionHarvest (Stop) dispatches a detached `memory update` once a session has substantive edits;
// with no spawn possible it blocks at most harvestMaxBlocks times asking for /memory:end.
func SessionHarvest(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	if !harvestable(in) {
		return ExitProceed
	}
	stamp := stampPath(in.SessionID)
	now := time.Now()
	st, ok := readStamp(stamp)
	switch {
	case !ok:
		edits := transcript.CountEdits(in.TranscriptPath)
		if edits < harvestMinEdits {
			return ExitProceed
		}
		if spawnHarvest(in.TranscriptPath, in.CWD, in.SessionID) {
			writeStamp(stamp, now, "bg", edits)
			return ExitProceed
		}
		writeStamp(stamp, now, "1", edits)
		return blockForHarvest(stderr, edits)
	case st.bg:
		if now.Sub(st.at) < reharvestCooldown {
			return ExitProceed // cheap gate before the whole-transcript count
		}
		if edits := transcript.CountEdits(in.TranscriptPath); edits >= st.edits+harvestRearmEdits &&
			spawnHarvest(in.TranscriptPath, in.CWD, in.SessionID) {
			writeStamp(stamp, now, "bg", edits)
		}
		return ExitProceed
	case memory.NewestMtime(memory.Dir(in.CWD)).After(st.at):
		writeStamp(stamp, now, "bg", st.edits) // /memory:end ran by hand: released, re-arms like a dispatch
		return ExitProceed
	case st.blocks >= harvestMaxBlocks:
		return ExitProceed // fail open
	}
	writeStamp(stamp, st.at, strconv.Itoa(st.blocks+1), st.edits)
	return blockForHarvest(stderr, st.edits)
}

// SessionHarvestEnd (SessionEnd) harvests the final state when edits since the last dispatch cross the floor.
func SessionHarvestEnd(r io.Reader) int {
	in := ParseInput(r)
	if !harvestable(in) {
		return ExitProceed
	}
	stamp := stampPath(in.SessionID)
	edits := transcript.CountEdits(in.TranscriptPath)
	covered := 0
	if st, ok := readStamp(stamp); ok {
		covered = st.edits
	}
	if edits >= harvestMinEdits && edits > covered && spawnHarvest(in.TranscriptPath, in.CWD, in.SessionID) {
		writeStamp(stamp, time.Now(), "bg", edits)
	}
	return ExitProceed
}

func harvestable(in *Input) bool {
	if in.SessionID == "" || in.TranscriptPath == "" || memory.IsHarvestChild() {
		return false
	}
	fi, err := os.Stat(in.TranscriptPath)
	return err == nil && !fi.IsDir()
}

func blockForHarvest(stderr io.Writer, edits int) int {
	fmt.Fprintf(stderr, "Session harvest (~%d file edits this session): the background worker could not be started. "+
		"Before stopping, run /memory:end to save the session to the memory bank.\n", edits)
	return ExitBlock
}

// spawnHarvest is the detached-worker seam; tests replace it.
var spawnHarvest = func(transcriptPath, cwd, sessionID string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	logPath := filepath.Join(memory.StateDir(), "harvest-"+time.Now().Format("20060102-150405")+".log")
	if os.MkdirAll(filepath.Dir(logPath), 0o700) != nil {
		return false
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	defer log.Close()
	args := []string{"memory", "update", "--transcript", transcriptPath, "--cwd", cwd, "--session", sessionID}
	if !proc.SpawnDetached(self, args, cwd, log) {
		return false
	}
	_ = memory.WriteHarvestResult(sessionID, memory.HarvestResult{Status: memory.HarvestPending, Time: time.Now(), Log: logPath})
	return true
}

// harvestStamp is "<RFC3339>|<bg|block count>|<edits covered>".
type harvestStamp struct {
	at     time.Time
	bg     bool
	blocks int
	edits  int
}

func stampPath(sessionID string) string {
	return filepath.Join(memory.StateDir(), "stamp-"+filepath.Base(sessionID))
}

func readStamp(path string) (harvestStamp, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return harvestStamp{}, false
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(parts) != 3 {
		return harvestStamp{}, false
	}
	at, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return harvestStamp{}, false
	}
	st := harvestStamp{at: at, bg: parts[1] == "bg"}
	st.blocks, _ = strconv.Atoi(parts[1])
	st.edits, _ = strconv.Atoi(parts[2])
	return st, true
}

func writeStamp(path string, at time.Time, state string, edits int) {
	_ = paths.AtomicWrite(path, []byte(at.Format(time.RFC3339)+"|"+state+"|"+strconv.Itoa(edits)), 0o600)
}
