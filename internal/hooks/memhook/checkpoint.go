package memhook

import (
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

// stewardPctEnv is the vault env for the context share that starts the handoff; 0 mutes the handoff only.
const stewardPctEnv = "HARNESS_CONTEXT_STEWARD_PCT"

// defaultStewardPct is the threshold for an unset or invalid stewardPctEnv.
const defaultStewardPct = 75

// Probes of work in flight; tests replace them.
var (
	sessionOwner   = inflight.SessionOwner
	countOwned     = inflight.CountOwned
	liveDispatches = inflight.LiveDispatches
)

// stewardPct is the threshold and whether the handoff is on (env 0 keeps the update, mutes the handoff).
func stewardPct() (pct int, handoff bool) {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(stewardPctEnv)))
	switch {
	case err != nil || n < 0 || n > 100:
		return defaultStewardPct, true
	case n == 0:
		return defaultStewardPct, false
	}
	return n, true
}

// contextPct is the share of the window the transcript's last turn fills.
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

// contextCheckpoint: once per crossing with nothing in flight, spawn one detached update that also writes a handoff.
func contextCheckpoint(in *core.Input) {
	if in.StopHookActive || !memory.Updatable(in.SessionID, in.TranscriptPath) {
		return
	}
	pct, ok := contextPct(in.SessionID, in.TranscriptPath)
	if !ok {
		return
	}
	threshold, handoff := stewardPct()
	flag := filepath.Join(paths.StateDir(), "checkpoint", filepath.Base(in.SessionID))
	if !claimCrossing(in, flag, pct, threshold) {
		return
	}
	// The fresh "bg" stamp makes the same Stop's sessionUpdate a no-op.
	if !memory.DispatchCheckpoint(updateRequest(in), handoff, time.Now()) { // under the stamp lock, like UpdateIfNew
		fmt.Fprintln(os.Stderr, "context-checkpoint: could not start the background handoff; retrying at the next Stop")
		_ = os.Remove(flag)
	}
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

// Stop runs the checkpoint and memory update; never blocks and never continues the turn.
func Stop(in *core.Input) core.Result {
	var r core.Result
	in = AgentInput(sessionRootInput(in)) // once per Stop: the checkpoint and the update share it
	contextCheckpoint(in)
	if sessionUpdate(in) {
		r.SystemMessage = updateNotSaved
	}
	return r
}
