// The release side of the dispatch cap: PostToolUseFailure, PostToolUse(Workflow) and SubagentStop retire the
// markers `agent-effort` claimed in internal/inflight; back-pressure, not a mutex.
package hooks

import (
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/proc"
)

// spawnTools are Claude Code's subagent-spawn tool names (Agent; Task on older builds).
var spawnTools = map[string]bool{"Agent": true, "Task": true}

// releaseSpawn retires the exact tool_use_id a spawn or Workflow claimed; a blocked dispatch wrote none, so it is a no-op.
func releaseSpawn(in *Input) Result {
	if owner, id := inflight.SessionOwner(), in.nested("tool_use_id"); owner != 0 && id != "" {
		inflight.Release(owner, id)
		clearPlanClaim(in.CWD, id) // a failed spawn never reaches SubagentStop
	}
	return Result{}
}

// subagentStopRunner retires the stopping agent's claim by agent id (first stop retires; a second is a no-op).
func subagentStopRunner(in *Input) Result {
	owner, id := inflight.SessionOwner(), in.SubagentID()
	if owner == 0 || id == "" {
		return Result{}
	}
	dirs := []string{inflight.SubagentDir(in.TranscriptPath)}
	if in.AgentTranscriptPath != "" {
		dirs = append([]string{filepath.Dir(in.AgentTranscriptPath)}, dirs...)
	}
	clearStoppedClaim(in, dirs, id) // before the retire: it reads the same meta.json
	for _, d := range dirs {
		if d != "" {
			inflight.RetireAgent(owner, d, id)
		}
	}
	return Result{}
}

// ownerDead is true only when the probe itself is trusted: off unix Alive is always false, and
// treating every pid as dead there would silently disable both this and the idle-harvest watcher.
func ownerDead(pid int) bool {
	return pid > 0 && proc.Alive(os.Getpid()) && !proc.Alive(pid)
}
