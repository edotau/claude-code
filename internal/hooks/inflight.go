// The release side of the dispatch cap: PostToolUseFailure, PostToolUse(Workflow) and SubagentStop retire the
// markers `agent-effort` claimed in internal/proc/inflight; back-pressure, not a mutex.
package hooks

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
)

// spawnTools are Claude Code's subagent-spawn tool names (Agent; Task on older builds).
var spawnTools = map[string]bool{"Agent": true, "Task": true}

// releaseSpawn retires the exact tool_use_id a spawn or Workflow claimed; a blocked dispatch wrote none, so it is a no-op.
func releaseSpawn(in *core.Input) core.Result {
	if owner, id := inflight.SessionOwner(), in.Nested("tool_use_id"); owner != 0 && id != "" {
		inflight.Release(owner, id)
		clearPlanClaim(in.CWD, id) // a failed spawn never reaches SubagentStop
	}
	return core.Result{}
}

// reapLeaf kills the stopping leaf's leftover background shells; set by leafreap.go where signals exist.
var reapLeaf = func(*core.Input) {}

// subagentStopRunner retires the stopping agent's claim by agent id (first stop retires; a second is a no-op).
func subagentStopRunner(in *core.Input) core.Result {
	owner, id := inflight.SessionOwner(), in.SubagentID()
	if owner == 0 || id == "" {
		return core.Result{}
	}
	dirs := []string{inflight.SubagentDir(in.TranscriptPath)}
	if in.AgentTranscriptPath != "" {
		dirs = append([]string{filepath.Dir(in.AgentTranscriptPath)}, dirs...)
	}
	note, blocked := leafStopBlock(in, id)
	if blocked {
		return note // a blocked stop retires nothing: the leaf is still running
	}
	reapLeaf(in)
	clearStoppedClaim(in, dirs, id) // before the retire: it reads the same meta.json
	for _, d := range dirs {
		if d != "" {
			inflight.RetireAgent(owner, d, id)
		}
	}
	return note
}
