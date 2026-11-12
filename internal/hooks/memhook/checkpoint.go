package memhook

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
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

// contextPct is the share of the window the transcript's last turn fills: Claude Code's reported size, else the model's.
func contextPct(session, transcriptPath string) (int, bool) {
	u, ok := transcript.LastUsage(transcriptPath)
	if !ok {
		return 0, false
	}
	window := transcript.RecordedWindow(session)
	if window <= 0 || window < u.Tokens {
		window = transcript.Window(u.Model, u.Tokens)
	}
	return u.Tokens * 100 / window, true
}

// inFlight reports background tasks, subagent markers or dispatches that a compaction would orphan.
func inFlight(in *core.Input, now time.Time) bool {
	owner := sessionOwner()
	return in.BackgroundTasks > 0 || countOwned(owner, now) > 0 || liveDispatches(owner) > 0
}

// handoffPrompt asks the model to write the handoff, then the owner to compact.
func handoffPrompt(in *core.Input, pct int) core.Result {
	dir := memory.HandoffsDir(memory.Dir(cwdOf(in)))
	return core.Result{
		Context: fmt.Sprintf("Context at %d%% with nothing in flight: write a handoff to %s/YYYY-MM-DD-<slug>.md "+
			"(State, In flight, Next), then ask the owner to run /compact.", pct, dir),
		SystemMessage: fmt.Sprintf("context %d%%: writing handoff; run /compact after", pct),
	}
}

// contextCheckpoint: once per crossing with nothing in flight, spawn an update and prompt a handoff; in has the root.
func contextCheckpoint(in *core.Input) core.Result {
	if in.StopHookActive || !memory.Updatable(in.SessionID, in.TranscriptPath) {
		return core.Result{}
	}
	pct, ok := contextPct(in.SessionID, in.TranscriptPath)
	if !ok {
		return core.Result{}
	}
	threshold, prompt := stewardPct()
	flag := filepath.Join(paths.StateDir(), "checkpoint", filepath.Base(in.SessionID))
	if !claimCrossing(in, flag, pct, threshold) {
		return core.Result{}
	}
	// The fresh "bg" stamp makes the same Stop's sessionUpdate a no-op.
	if !memory.DispatchCheckpoint(updateRequest(in), time.Now()) { // under the stamp lock, like UpdateIfNew
		fmt.Fprintln(os.Stderr, "context-checkpoint: could not start the background handoff; retrying at the next Stop")
		_ = os.Remove(flag)
		return core.Result{}
	}
	if !prompt {
		return core.Result{}
	}
	return handoffPrompt(in, pct)
}

// claimCrossing writes flag for a first crossing of threshold with nothing in flight; below it, it re-arms.
func claimCrossing(in *core.Input, flag string, pct, threshold int) bool {
	_, statErr := os.Stat(flag)
	if pct < threshold {
		if statErr == nil { // re-arm after a compaction or clear
			_ = os.Remove(flag)
		}
		return false
	}
	if statErr == nil || inFlight(in, time.Now()) {
		return false
	}
	return createFlag(flag)
}

// createFlag creates flag exclusively; false when it exists (a concurrent Stop won) or cannot be written.
func createFlag(flag string) bool {
	if err := os.MkdirAll(filepath.Dir(flag), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "context-checkpoint: %v\n", err)
		return false
	}
	f, err := os.OpenFile(flag, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // O_EXCL: a concurrent Stop loses the race
	if err != nil {
		if !os.IsExist(err) {
			fmt.Fprintf(os.Stderr, "context-checkpoint: write flag %s: %v\n", flag, err)
		}
		return false
	}
	_ = f.Close()
	return true
}

// Stop runs the context checkpoint and memory update; only the memory update can block the stop.
func Stop(in *core.Input) core.Result {
	var hv bytes.Buffer
	in = AgentInput(sessionRootInput(in)) // once per Stop: the checkpoint and the update share it
	steward := contextCheckpoint(in)
	code, failedOpen := sessionUpdate(in, &hv)
	r := core.ExitResult(code, hv.String())
	if failedOpen {
		r.SystemMessage = updateNotSaved
	}
	r.Context = core.JoinContext(r.Context, steward.Context)
	r.SystemMessage = core.JoinNonEmpty("; ", r.SystemMessage, steward.SystemMessage)
	return r
}
