// The dispatch group's PreToolUse runner: cap → pin → claim on one Agent/Task/Workflow spawn, so a blocked
// dispatch claims nothing; the post-side runners live in inflight.go.
package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/job"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// agentCallRe finds `agent(` call sites in a Workflow script.
var agentCallRe = regexp.MustCompile(`(?:^|[^\w$.])agent\s*\(`)

// Effort: the cap check, then the pin step, then the claim (a Workflow claims one slot per agent() site).
func Effort(in *core.Input) core.Result {
	if !core.SpawnTools[in.ToolName] && in.ToolName != "Workflow" {
		return core.Result{}
	}
	owner := inflight.SessionOwner()
	if owner == 0 {
		return agentEffortResult(in) // an unidentifiable owner claims nothing: fail open
	}
	account, lim := router.SessionAccount(), router.CurrentLimits()
	claim := inflight.Claim{Owner: owner, ID: in.Nested("tool_use_id"), Account: account, N: 1, Transcript: in.TranscriptPath}
	if claim.ID == "" {
		claim.ID = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if ok, snap := inflight.Fits(claim, lim); !ok {
		return blockAtCap(snap, account, lim)
	}
	res := agentEffortResult(in)
	if res.Decision == core.Block {
		return res
	}
	if in.ToolName == "Workflow" {
		claim.N = min(max(workflowLeaves(in, res), 1), lim.For(account), lim.Total)
	}
	if ok, snap := inflight.TryClaim(claim, lim); !ok {
		return blockAtCap(snap, account, lim)
	}
	docs.ClaimPlanTask(in, claim.ID)
	return res
}

func blockAtCap(snap inflight.Snapshot, account string, lim inflight.Limits) core.Result {
	return core.Result{Decision: core.Block, Reason: fmt.Sprintf("agent-effort: %d agents already in flight (cap %d for %s, %d machine-wide) — "+
		"wait for one to finish or run this task sequentially.", snap.Per[account], lim.For(account), account, lim.Total)}
}

// workflowLeaves counts agent() call sites in the script the Workflow will run: the pin step's rewrite, else the original.
func workflowLeaves(in *core.Input, res core.Result) int {
	script, _ := res.UpdatedInput["script"].(string)
	if script == "" {
		script = in.Nested("tool_input", "script")
	}
	if script == "" {
		if path := in.Nested("tool_input", "scriptPath"); path != "" {
			if data, err := os.ReadFile(path); err == nil {
				script = string(data)
			}
		}
	}
	return len(agentCallRe.FindAllStringIndex(script, -1))
}

// The release side of the dispatch cap: PostToolUseFailure, PostToolUse(Workflow) and SubagentStop retire the
// markers `agent-effort` claimed in internal/proc/inflight; back-pressure, not a mutex.

// Release retires the exact tool_use_id a spawn or Workflow claimed; a blocked dispatch wrote none, so it is a no-op.
func Release(in *core.Input) core.Result {
	if owner, id := inflight.SessionOwner(), in.Nested("tool_use_id"); owner != 0 && id != "" {
		inflight.Release(owner, id)
		docs.ClearPlanClaim(in.CWD, id) // a failed spawn never reaches SubagentStop
	}
	return core.Result{}
}

// reapLeaf kills the stopping leaf's leftover background shells; set by leafreap.go where signals exist.
var reapLeaf = func(*core.Input) {}

// Stop retires the stopping agent's claim by agent id (first stop retires; a second is a no-op).
func Stop(in *core.Input) core.Result {
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
	docs.ClearStoppedClaim(in, dirs, id) // before the retire: it reads the same meta.json
	for _, d := range dirs {
		if d != "" {
			inflight.RetireAgent(owner, d, id)
		}
	}
	return note
}

// The leaf-stop gate: a leaf may not stop while its background dispatch runs; a coworker needs a ran-on line from a tool result.

// leafStopMaxWait bounds the wait from the dispatch's start, so a hung job cannot trap a leaf.
const leafStopMaxWait = 2 * time.Hour

// loadDispatch reads state/dispatch/<id>.json by its exact id (LoadRecord also takes a prefix); tests replace it.
var loadDispatch = func(id string) (job.Record, error) {
	r, err := job.LoadRecord(id)
	if err == nil && r.ID != id {
		err = fmt.Errorf("dispatch %q: no exact match", id)
	}
	return r, err
}

// leafJobIDs lists the job ids the leaf's Bash tool results printed, in order; an unreadable transcript is an error.
func leafJobIDs(path string) ([]string, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return transcript.DispatchIDs(path), nil
}

// leafStopBlock returns a Block while a dispatch the stopping leaf started is alive; any error fails open.
func leafStopBlock(in *core.Input, agentID string) (core.Result, bool) {
	if in.Agent() != core.AgentClaude || in.AgentTranscriptPath == "" {
		return core.Result{}, false
	}
	ids, err := leafJobIDs(in.AgentTranscriptPath)
	if err != nil {
		return core.Result{}, false
	}
	expired, block := liveDispatchVerdict(ids, agentID)
	if block {
		return expired, true
	}
	if res, block := coworkerRanOnVerdict(in, agentID); block || expired.SystemMessage == "" {
		return res, block
	}
	return expired, false
}

// liveDispatchVerdict blocks on the first live dispatch within its wait limit, else carries the first expiry note.
func liveDispatchVerdict(ids []string, agentID string) (expired core.Result, block bool) {
	for _, id := range ids {
		rec, err := loadDispatch(id)
		if err != nil || !inflight.DispatchAlive(rec.Status, rec.PID, rec.Time) {
			continue
		}
		res, block := leafStopVerdict(agentID, rec)
		if block {
			return res, true
		}
		if expired.SystemMessage == "" {
			expired = res
		}
	}
	return expired, false
}

// leafStopVerdict blocks until the wait limit, then allows the stop with a visible note.
func leafStopVerdict(agentID string, rec job.Record) (core.Result, bool) {
	if time.Since(rec.Time) >= leafStopMaxWait {
		return core.Result{SystemMessage: fmt.Sprintf("leaf %s stopped with dispatch %s still running (%s limit reached)", agentID, rec.ID, leafStopMaxWait)}, false
	}
	return core.Result{Decision: core.Block, Reason: fmt.Sprintf("Dispatch %s you started is still running. Run `claude-code agents tail -f %s` "+
		"with a 600000ms Bash timeout, repeating until it finishes, then report its result; do not stop before it reaches a terminal status.", rec.ID, rec.ID)}, true
}

// ranOnPattern matches the `ask: ran on` stderr line or the --format json "ran_on" field, as a tool result printed it.
var ranOnPattern = regexp.MustCompile(`ask: ran on |\\"ran_on\\":`)

// coworkerGateMark opens the coworker gate's reason; a tool result carrying it means the leaf was already gated once.
const coworkerGateMark = "No tool result of yours carries an `ask: ran on` line"

// handedBackMark is SubagentHandback's success result: the run is over, so a later stop block cannot reopen it.
const handedBackMark = "Report delivered to your caller"

// leafScan is what the gates read from a leaf transcript: its brief and which tool results it has seen.
type leafScan struct {
	brief                    string
	ranOn, gated, handedBack bool
}

// leafBrief scans a leaf transcript for its first user message and the ran-on, gate and hand-back tool results.
func leafBrief(path string) (s leafScan, err error) {
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	err = textutil.EachLineErr(f, func(b []byte) {
		var m struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(b, &m) != nil {
			return
		}
		if s.brief == "" && m.Type == "user" {
			s.brief = transcript.ContentText(m.Message.Content)
		}
		line := string(b)
		if !strings.Contains(line, `"tool_result"`) {
			return
		}
		s.ranOn = s.ranOn || ranOnPattern.MatchString(line)
		s.gated = s.gated || strings.Contains(line, coworkerGateMark)
		s.handedBack = s.handedBack || strings.Contains(line, handedBackMark)
	})
	return s, err
}

// leafAgentType is the stopping leaf's type: the payload's agent_type, else the meta.json beside its transcript.
func leafAgentType(in *core.Input) string {
	if t := in.Nested("agent_type"); t != "" {
		return t
	}
	b, err := os.ReadFile(strings.TrimSuffix(in.AgentTranscriptPath, ".jsonl") + ".meta.json")
	if err != nil {
		return ""
	}
	var m struct {
		AgentType string `json:"agentType"`
	}
	_ = json.Unmarshal(b, &m)
	return m.AgentType
}

// requestsAgent is true when the brief asks for an external agent; `Agent: in-context` and a brief with no line do not.
func requestsAgent(brief string) bool {
	switch {
	case inContextBrief(brief):
		return false
	case strings.Contains(brief, "Routed by agent-effort"):
		return true
	}
	return agentLineRe.MatchString(brief) || briefProfile(brief) != ""
}

// pinnedAgentType is true for a leaf type fixed to one external agent; its type is the request, whatever the brief says.
func pinnedAgentType(t string) bool {
	return t == "code-worker:codex" || t == "code-worker:gemini" || t == "coworker:copilot" || t == "coworker:opencode"
}

// ranOnGated is true for the leaves that can call an external agent: coworker, coworker:*, code-worker, code-worker:codex|gemini.
func ranOnGated(t string) bool {
	return t == "coworker" || strings.HasPrefix(t, "coworker:") || t == "code-worker" || t == "code-worker:codex" || t == "code-worker:gemini"
}

// coworkerRanOnVerdict blocks a coworker's stop or hand-back once when no ran-on line was seen; a repeat passes.
func coworkerRanOnVerdict(in *core.Input, agentID string) (core.Result, bool) {
	if !ranOnGated(leafAgentType(in)) {
		return core.Result{}, false
	}
	s, err := leafBrief(in.AgentTranscriptPath)
	if err != nil || s.ranOn || (!requestsAgent(s.brief) && !pinnedAgentType(leafAgentType(in))) {
		return core.Result{}, false
	}
	if in.StopHookActive || s.gated || s.handedBack {
		return core.Result{SystemMessage: fmt.Sprintf("coworker %s: finished with no ran-on line", agentID)}, false
	}
	return core.Result{Decision: core.Block, Reason: coworkerGateMark + " (or a `ran_on` field), so the external agent was never " +
		"called. Run the `claude-code agents ask` the brief requests, or report BLOCKED with the reason; do not do the work in-context."}, true
}

// Handback runs the stop gates on SubagentHandback, which ends a background leaf before SubagentStop can block it.
func Handback(in *core.Input) core.Result {
	id := in.SubagentID()
	if id == "" || in.ToolName != "SubagentHandback" {
		return core.Result{}
	}
	leaf := *in
	if leaf.AgentTranscriptPath == "" {
		leaf.AgentTranscriptPath = filepath.Join(inflight.SubagentDir(in.TranscriptPath), "agent-"+id+".jsonl")
	}
	res, _ := leafStopBlock(&leaf, id)
	return res
}
