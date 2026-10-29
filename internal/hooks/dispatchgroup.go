// The dispatch group's PreToolUse runner: cap → pin → claim on one Agent/Task/Workflow spawn, so a blocked
// dispatch claims nothing; the post-side runners live in inflight.go.
package hooks

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
)

// agentCallRe finds `agent(` call sites in a Workflow script.
var agentCallRe = regexp.MustCompile(`(?:^|[^\w$.])agent\(`)

// dispatchPre: the cap check, then the pin step, then the claim (a Workflow claims one slot per agent() site).
func dispatchPre(in *Input) Result {
	if !spawnTools[in.ToolName] && in.ToolName != "Workflow" {
		return Result{}
	}
	owner := inflight.SessionOwner()
	if owner == 0 {
		return agentEffortResult(in) // an unidentifiable owner claims nothing: fail open
	}
	account, lim := inflight.SessionAccount(), inflight.CurrentLimits()
	claim := inflight.Claim{Owner: owner, ID: in.nested("tool_use_id"), Account: account, N: 1, Transcript: in.TranscriptPath}
	if claim.ID == "" {
		claim.ID = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if ok, snap := inflight.Fits(claim, lim); !ok {
		return blockAtCap(snap, account, lim)
	}
	res := agentEffortResult(in)
	if res.Decision == Block {
		return res
	}
	if in.ToolName == "Workflow" {
		claim.N = min(max(workflowLeaves(in, res), 1), lim.For(account), lim.Total)
	}
	if ok, snap := inflight.TryClaim(claim, lim); !ok {
		return blockAtCap(snap, account, lim)
	}
	claimPlanTask(in)
	return res
}

func blockAtCap(snap inflight.Snapshot, account string, lim inflight.Limits) Result {
	return Result{Decision: Block, Reason: fmt.Sprintf("agent-effort: %d agents already in flight (cap %d for %s, %d machine-wide) — "+
		"wait for one to finish or run this task sequentially.", snap.Per[account], lim.For(account), account, lim.Total)}
}

// workflowLeaves counts agent() call sites in the script the Workflow will run: the pin step's rewrite, else the original.
func workflowLeaves(in *Input, res Result) int {
	script, _ := res.UpdatedInput["script"].(string)
	if script == "" {
		script = in.nested("tool_input", "script")
	}
	if script == "" {
		if path := in.nested("tool_input", "scriptPath"); path != "" {
			if data, err := os.ReadFile(path); err == nil {
				script = string(data)
			}
		}
	}
	return len(agentCallRe.FindAllStringIndex(script, -1))
}

// claimPlanTask is the one call point where the plans lane (Phase 3, internal/plans claim API) wires live task claims.
func claimPlanTask(*Input) {}
