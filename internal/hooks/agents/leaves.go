// The dispatch group's PreToolUse runner: cap → pin → claim on one Agent/Task/Workflow spawn, so a blocked
// dispatch claims nothing; the post-side runners (Release, Stop) follow Effort below.
package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/agentid"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// agentCallRe finds `agent(` call sites in a Workflow script.
var agentCallRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?:^|[^\w$.])agent\s*\(`) })

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
	return core.WithStderr(res, docs.ClaimPlanTask(in, claim.ID))
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
	return len(agentCallRe().FindAllStringIndex(script, -1))
}

// The release side of the dispatch cap: PostToolUseFailure, PostToolUse(Workflow) and SubagentStop retire the
// markers `agent-effort` claimed in internal/inflight; back-pressure, not a mutex.

// Release retires the exact tool_use_id a spawn or Workflow claimed; a blocked dispatch wrote none, so it is a no-op.
func Release(in *core.Input) core.Result {
	if owner, id := inflight.SessionOwner(), in.Nested("tool_use_id"); owner != 0 && id != "" {
		inflight.Release(owner, id)
		return core.WithStderr(core.Result{}, docs.ClearPlanClaim(in.CWD, id)) // a failed spawn never reaches SubagentStop
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
	clearErr := docs.ClearStoppedClaim(in, dirs, id) // before the retire: it reads the same meta.json
	for _, d := range dirs {
		if d != "" {
			inflight.RetireAgent(owner, d, id)
		}
	}
	return core.WithStderr(note, clearErr)
}

// The leaf-stop gate: a leaf may not stop while its background dispatch runs; an external-agent leaf needs a ran-on line from a tool result.

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
	if res, block := ranOnVerdict(in, agentID); block || expired.SystemMessage == "" {
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

// ranOnPattern matches the `ask: ran on <agent> · ` stderr line or the json "ran_on" field; docs quoting it do not match.
var ranOnPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`ask: ran on [\w.-]+ · |"ran_on":\s*"ran on [\w.-]+ · `)
})

// ranOnGateMark opens the ran-on gate's reason; a hook block carrying it counts as one prior gate.
const ranOnGateMark = "No tool result of yours carries an `ask: ran on` line"

// ranOnMaxBlocks is how many times the gate blocks one leaf before it fails open with a visible note.
const ranOnMaxBlocks = 3

// handedBackMark is SubagentHandback's success result: the run is over, so a later stop block cannot reopen it.
const handedBackMark = "Report delivered to your caller"

// leafScan is what the gates read from a leaf transcript: its brief, the ran-on and hand-back results, and prior gate blocks.
type leafScan struct {
	brief             string
	ranOn, handedBack bool
	gates             int
}

// leafToolResult is one tool_result item of a user message.
type leafToolResult struct {
	Type    string          `json:"type"`
	IsError bool            `json:"is_error"`
	Content json.RawMessage `json:"content"`
}

// leafBrief scans a leaf transcript for its first user message, tool results and hook blocks; assistant text never counts.
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
		if json.Unmarshal(b, &m) != nil || m.Type != "user" {
			return
		}
		if s.brief == "" {
			s.brief = transcript.ContentText(m.Message.Content)
			return
		}
		var items []leafToolResult
		if json.Unmarshal(m.Message.Content, &items) != nil { // a plain-text user turn: Stop/SubagentStop hook feedback
			if text := transcript.ContentText(m.Message.Content); strings.Contains(text, "hook feedback:") && strings.Contains(text, ranOnGateMark) {
				s.gates++
			}
			return
		}
		for _, it := range items {
			if it.Type == "tool_result" {
				s.scanResult(it)
			}
		}
	})
	return s, err
}

// scanResult reads one tool result: a ran-on line, a delivered hand-back, or a denied hand-back (a PreToolUse hook error).
func (s *leafScan) scanResult(it leafToolResult) {
	text := transcript.ContentText(it.Content)
	s.ranOn = s.ranOn || ranOnPattern().MatchString(text)
	if it.IsError {
		if strings.Contains(text, " hook error: ") && strings.Contains(text, ranOnGateMark) {
			s.gates++
		}
		return
	}
	s.handedBack = s.handedBack || strings.Contains(text, handedBackMark)
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
	return agentLineRe().MatchString(brief) || briefProfile(brief) != ""
}

// externalTypes are the leaf types named for the external agent they request; the type is the request.
var externalTypes = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range agentid.External() {
		m[n] = true
	}
	return m
}()

// labelAgent is true for an agent a dispatch label may name (claude or an external type), and a type that carries one.
func labelAgent(name string) bool { return name == agentid.Claude || externalTypes[name] }

// coworkerAgents maps each coworker leaf type to the agent it runs when its brief names none; the type is the request.
var coworkerAgents = map[string]string{"coworker": "copilot", "coworker:copilot": "copilot",
	"coworker:opencode": "opencode", "coworker:databricks": "claude"}

// coworkerPin is the agent a pinned coworker type always runs, whatever its brief's Agent: line says ("" if unpinned).
func coworkerPin(t string) string {
	if t == "coworker:copilot" || t == "coworker:opencode" {
		return coworkerAgents[t]
	}
	return ""
}

// pinnedAgentType is true for a leaf type whose type is the request (an external agent or a coworker), whatever the brief says.
func pinnedAgentType(t string) bool {
	return externalTypes[t] || coworkerAgents[t] != ""
}

// ranOnVerdict blocks a gated leaf's stop or hand-back up to ranOnMaxBlocks times when no ran-on line was seen.
func ranOnVerdict(in *core.Input, agentID string) (core.Result, bool) {
	t := leafAgentType(in)
	if !pinnedAgentType(t) { // only leaves that can call an external agent: codex|copilot|gemini|opencode, coworker[:*]
		return core.Result{}, false
	}
	s, err := leafBrief(in.AgentTranscriptPath)
	if err != nil || s.ranOn || (!requestsAgent(s.brief) && !pinnedAgentType(t)) {
		return core.Result{}, false
	}
	if in.StopHookActive && s.gates == 0 { // a prior block the scan cannot read: fail open rather than trap the leaf
		s.gates = ranOnMaxBlocks
	}
	if s.handedBack || s.gates >= ranOnMaxBlocks {
		return core.Result{SystemMessage: fmt.Sprintf("leaf %s: finished with no ran-on line", agentID)}, false
	}
	return core.Result{Decision: core.Block, Reason: fmt.Sprintf("%s (or a `ran_on` field), so the external agent was never called "+
		"(block %d of %d). Run the `claude-code agents ask` the brief requests, or report BLOCKED with the reason; read-only work "+
		"still runs on the external agent (`--read-only`); never do it in-context.", ranOnGateMark, s.gates+1, ranOnMaxBlocks)}, true
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
