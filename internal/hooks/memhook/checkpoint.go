package memhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

// stewardPctEnv is the vault env for the context share that starts the handoff; 0 mutes the prompt only.
const stewardPctEnv = "HARNESS_CONTEXT_STEWARD_PCT"

// defaultStewardPct is the threshold for an unset or invalid stewardPctEnv.
const defaultStewardPct = 75

// Probes of work in flight; tests replace them.
var (
	sessionOwner   = inflight.SessionOwner
	countOwned     = inflight.CountOwned
	liveDispatches = inflight.LiveDispatches
)

// stewardPct is the threshold and whether the handoff prompt is on (env 0 keeps the update, mutes the prompt).
func stewardPct() (pct int, prompt bool) {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(stewardPctEnv)))
	switch {
	case err != nil || n < 0 || n > 100:
		return defaultStewardPct, true
	case n == 0:
		return defaultStewardPct, false
	}
	return n, true
}

// contextPct is the share of the model window the transcript's last turn fills.
func contextPct(transcriptPath string) (int, bool) {
	u, ok := transcript.LastUsage(transcriptPath)
	if !ok {
		return 0, false
	}
	return u.Tokens * 100 / transcript.Window(u.Model, u.Tokens), true
}

// inFlight reports background tasks, subagent markers or dispatches that a compaction would orphan.
func inFlight(in *core.Input, now time.Time) bool {
	owner := sessionOwner()
	return in.BackgroundTasks > 0 || countOwned(owner, now) > 0 || liveDispatches(owner) > 0
}

// handoffPrompt asks the model to write the handoff, then the owner to compact.
func handoffPrompt(in *core.Input, pct int) core.Result {
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	dir := memory.HandoffsDir(memory.Dir(cwd))
	return core.Result{
		Context: fmt.Sprintf("Context at %d%% with nothing in flight: write a handoff to %s/YYYY-MM-DD-<slug>.md "+
			"(State, In flight, Next), then ask the owner to run /compact.", pct, dir),
		SystemMessage: fmt.Sprintf("context %d%%: writing handoff; run /compact after", pct),
	}
}

// contextCheckpoint: once per threshold crossing with nothing in flight, spawn an update and prompt a handoff.
func contextCheckpoint(in *core.Input) core.Result {
	in = sessionRootInput(in)
	if in.StopHookActive || !updatable(in) {
		return core.Result{}
	}
	pct, ok := contextPct(in.TranscriptPath)
	if !ok {
		return core.Result{}
	}
	threshold, prompt := stewardPct()
	flag := filepath.Join(paths.StateDir(), "checkpoint", filepath.Base(in.SessionID))
	_, statErr := os.Stat(flag)
	if pct < threshold {
		if statErr == nil { // re-arm after a compaction or clear
			_ = os.Remove(flag)
		}
		return core.Result{}
	}
	if statErr == nil || inFlight(in, time.Now()) {
		return core.Result{}
	}
	if err := os.MkdirAll(filepath.Dir(flag), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "context-checkpoint: %v\n", err)
		return core.Result{}
	}
	f, err := os.OpenFile(flag, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // O_EXCL: a concurrent Stop loses the race
	if err != nil {
		if !os.IsExist(err) {
			fmt.Fprintf(os.Stderr, "context-checkpoint: write flag %s: %v\n", flag, err)
		}
		return core.Result{}
	}
	_ = f.Close()
	// The fresh "bg" stamp makes the same Stop's sessionUpdate a no-op.
	if !dispatchUpdate(in, triggerStop, countEdits(in.TranscriptPath, countPath(in.SessionID)), time.Now()) {
		fmt.Fprintln(os.Stderr, "context-checkpoint: could not start the background handoff; retrying at the next Stop")
		_ = os.Remove(flag)
		return core.Result{}
	}
	if !prompt {
		return core.Result{}
	}
	return handoffPrompt(in, pct)
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

// writeCompactRecord notes the compaction for the next start; errors go to stderr only.
func writeCompactRecord(in *core.Input, now time.Time) {
	pct, _ := contextPct(in.TranscriptPath)
	b, err := json.Marshal(compactRecord{Trigger: in.Trigger, Pct: pct, At: now.Format(time.RFC3339)})
	if err == nil {
		path := compactRecordPath(in.SessionID)
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

// resumeNote is the compact/clear start line: the compaction record and the fresh handoff, whichever exist.
func resumeNote(in *core.Input, cwd string, now time.Time) string {
	var parts []string
	if in.Source == "compact" {
		if r, ok := takeCompactRecord(in.SessionID); ok {
			parts = append(parts, compactedText(r))
		}
	}
	if h := newestHandoff(memory.HandoffsDir(memory.Dir(cwd)), now, 24*time.Hour); h != "" {
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

// Stop runs the context checkpoint and memory update; only the memory update can block the stop.
func Stop(in *core.Input) core.Result {
	var hv bytes.Buffer
	steward := contextCheckpoint(in)
	code, failedOpen := sessionUpdate(in, &hv)
	r := core.ExitResult(code, hv.String())
	if failedOpen {
		r.SystemMessage = updateNotSaved
	}
	r.Context = core.JoinContext(r.Context, steward.Context)
	r.SystemMessage = joinMessages(r.SystemMessage, steward.SystemMessage)
	return r
}

// joinMessages joins the non-empty user-visible notes with "; ".
func joinMessages(msgs ...string) string {
	return strings.Join(slices.DeleteFunc(slices.Clone(msgs), func(m string) bool { return m == "" }), "; ")
}
