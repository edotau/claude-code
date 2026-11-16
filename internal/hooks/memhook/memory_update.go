package memhook

import (
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/worktree"
)

// updateNotSaved is the visible line when the Stop hook proceeds without an update.
const updateNotSaved = "memory update NOT saved; run `claude-code memory update`"

// sessionUpdate spawns a detached update; a failed spawn proceeds with a note and SessionEnd/PreCompact retry.
func sessionUpdate(in *core.Input) (notSaved bool) {
	memory.RefreshStaleEmbeddings(in.CWD)
	if !memory.Updatable(in.SessionID, in.TranscriptPath) {
		return false
	}
	g := memory.UpdateIfNew(updateRequest(in), memory.TriggerStop, time.Now())
	return g.FailedOpen || g.Blocked
}

// End (SessionEnd) updates the final state when total work crosses the floor and exceeds the last dispatch's count.
func End(in *core.Input) core.Result {
	in = AgentInput(sessionRootInput(in))
	memory.RefreshStaleEmbeddings(in.CWD)
	if !memory.Updatable(in.SessionID, in.TranscriptPath) {
		return core.Result{}
	}
	memory.UpdateIfNew(updateRequest(in), memory.TriggerSessionEnd, time.Now())
	return core.Result{}
}

// Compact (PreCompact) updates any work no earlier dispatch covered before the context compacts; it never blocks.
func Compact(in *core.Input) core.Result {
	in = AgentInput(sessionRootInput(in))
	if !memory.Updatable(in.SessionID, in.TranscriptPath) {
		return core.Result{}
	}
	pct, _ := contextPct(in.SessionID, in.TranscriptPath)
	memory.WriteCompactRecord(in.SessionID, in.Trigger, pct, time.Now())
	memory.UpdateIfNew(updateRequest(in), memory.TriggerPreCompact, time.Now())
	return core.Result{}
}

// updateRequest is in's update ask; each dispatch logs to the session's linked-worktree ledger.
func updateRequest(in *core.Input) memory.UpdateRequest {
	return memory.UpdateRequest{SessionID: in.SessionID, Transcript: in.TranscriptPath, CWD: in.CWD,
		OnDispatch: func(trigger string, work int, now time.Time) {
			logUpdateToLedger(in.CWD, in.SessionID, trigger, work, now)
		}}
}

// logUpdateToLedger notes an update in the session's linked-worktree ledger; best effort, never fatal.
func logUpdateToLedger(cwd, sessionID, trigger string, work int, now time.Time) {
	_, _ = worktree.AppendLedger(cwd, fmt.Sprintf("update (%s): %d edits, session %s", trigger, work, sessionID), now)
}

// sessionRoot reads the session's recorded root; tests count it.
var sessionRoot = memory.SessionRoot

func sessionRootInput(in *core.Input) *core.Input {
	copy := *in
	copy.CWD = sessionRoot(in.SessionID, in.CWD)
	return &copy
}
