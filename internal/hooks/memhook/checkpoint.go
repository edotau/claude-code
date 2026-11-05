package memhook

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

// checkpointPct is the context-usage share that dispatches a background handoff once per session.
const checkpointPct = 85

// contextCheckpoint spawns a detached memory update once per session at ≥85% context; it never blocks Stop.
func contextCheckpoint(in *core.Input) {
	in = sessionRootInput(in)
	if in.StopHookActive || !updatable(in) {
		return
	}
	flag := filepath.Join(paths.StateDir(), "checkpoint", filepath.Base(in.SessionID))
	if _, err := os.Stat(flag); err == nil {
		return
	}
	u, ok := transcript.LastUsage(in.TranscriptPath)
	if !ok || u.Tokens*100/transcript.Window(u.Model, u.Tokens) < checkpointPct {
		return
	}
	if err := os.MkdirAll(filepath.Dir(flag), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "context-checkpoint: %v\n", err)
		return
	}
	f, err := os.OpenFile(flag, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // O_EXCL: a concurrent Stop loses the race
	if err != nil {
		if !os.IsExist(err) {
			fmt.Fprintf(os.Stderr, "context-checkpoint: write flag %s: %v\n", flag, err)
		}
		return
	}
	_ = f.Close()
	// The fresh "bg" stamp makes the same Stop's sessionUpdate a no-op.
	if !dispatchUpdate(in, triggerStop, countEdits(in.TranscriptPath, countPath(in.SessionID)), time.Now()) {
		fmt.Fprintln(os.Stderr, "context-checkpoint: could not start the background handoff; retrying at the next Stop")
		_ = os.Remove(flag)
	}
}

// Stop runs the context checkpoint and memory update; only the memory update can block the stop.
func Stop(in *core.Input) core.Result {
	var hv bytes.Buffer
	contextCheckpoint(in)
	code, failedOpen := sessionUpdate(in, &hv)
	r := core.ExitResult(code, hv.String())
	if failedOpen {
		r.SystemMessage = updateNotSaved
	}
	return r
}
