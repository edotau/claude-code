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

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/proc"
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
	return core.Result{Decision: core.Block, Reason: fmt.Sprintf("dispatch-effort: %d agents already in flight (cap %d for %s, %d machine-wide) — "+
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
// markers `dispatch-effort` claimed in internal/inflight; back-pressure, not a mutex.

// Release retires the exact tool_use_id a spawn or Workflow claimed; a blocked dispatch wrote none, so it is a no-op.
func Release(in *core.Input) core.Result {
	if owner, id := inflight.SessionOwner(), in.Nested("tool_use_id"); owner != 0 && id != "" {
		inflight.Release(owner, id)
		return core.WithStderr(core.Result{}, docs.ClearPlanClaim(in.CWD, id)) // a failed spawn never reaches SubagentStop
	}
	return core.Result{}
}

// reapLeaf kills the stopping leaf's leftover background shells; a seam for tests.
var reapLeaf = reapLeafBackground

// Stop retires the stopping agent's claim by agent id (first stop retires; a second is a no-op).
func Stop(in *core.Input) core.Result {
	id := in.SubagentID()
	if id == "" {
		return core.Result{}
	}
	lr := readLeaf(in.AgentTranscriptPath, leafWants(in))
	note, blocked := leafGate(in, id, lr)
	owner := inflight.SessionOwner()
	if blocked || owner == 0 { // the gates need no owner; only the claim retire does
		return note // a blocked stop retires nothing: the leaf is still running
	}
	dirs := []string{inflight.SubagentDir(in.TranscriptPath)}
	if in.AgentTranscriptPath != "" {
		dirs = append([]string{filepath.Dir(in.AgentTranscriptPath)}, dirs...)
	}
	clearErr := docs.ClearStoppedClaim(in, dirs, id) // before the retire: it reads the same meta.json
	for _, d := range dirs {
		if d != "" {
			inflight.RetireAgent(owner, d, id)
		}
	}
	reapLeaf(in, lr) // last: a reap that outruns the host timeout must not strand the claim and the slot
	return core.WithStderr(note, clearErr)
}

// The leaf-stop gate: a leaf may not stop while its background dispatch runs; an external-agent leaf needs a ran-on line from a tool result.

// leafStopMaxWait bounds the wait from the dispatch's start, so a hung job cannot trap a leaf.
const leafStopMaxWait = 2 * time.Hour

// loadDispatch reads state/dispatch/<id>.json by its exact id (LoadRecord also takes a prefix); tests replace it.
var loadDispatch = func(id string) (agent.Record, error) {
	r, err := agent.LoadRecord(id)
	if err == nil && r.ID != id {
		err = fmt.Errorf("dispatch %q: no exact match", id)
	}
	return r, err
}

// leafWant selects what one pass over a leaf transcript collects.
type leafWant struct{ ids, brief, files bool }

// leafWants is what the stop gates and the reaper read from the stopping leaf's transcript.
func leafWants(in *core.Input) leafWant {
	return leafWant{ids: in.Agent() == core.AgentClaude, brief: externalTypes[leafAgentType(in)], files: true}
}

// leafRead is one pass over a leaf transcript: its dispatch ids, brief scan and background output files.
type leafRead struct {
	ids, files []string
	scan       leafScan
	openErr    error // the transcript could not be opened
	scanErr    error // reading stopped early: scan is partial and files are dropped
}

// readLeaf reads the transcript once; an empty path reads nothing and reports an open error.
func readLeaf(path string, w leafWant) (r leafRead) {
	if path == "" {
		r.openErr = os.ErrNotExist
		return r
	}
	f, err := os.Open(path)
	if err != nil {
		r.openErr = err
		return r
	}
	defer f.Close()
	seenIDs, seenFiles := map[string]bool{}, map[string]bool{}
	r.scanErr = textutil.EachLineErr(f, func(b []byte) {
		if w.ids {
			r.ids = transcript.AppendDispatchIDs(r.ids, seenIDs, b)
		}
		if w.files {
			r.files = appendOutputFiles(r.files, seenFiles, b)
		}
		if w.brief {
			r.scan.addLine(b)
		}
	})
	if r.scanErr != nil {
		r.files = nil // an unreadable transcript reaps nothing
	}
	return r
}

// leafStopBlock is leafGate over a fresh read of the leaf's transcript.
func leafStopBlock(in *core.Input, agentID string) (core.Result, bool) {
	return leafGate(in, agentID, readLeaf(in.AgentTranscriptPath, leafWants(in)))
}

// leafGate returns a Block while a dispatch the stopping leaf started is alive; any error fails open.
func leafGate(in *core.Input, agentID string, lr leafRead) (core.Result, bool) {
	if in.Agent() != core.AgentClaude || in.AgentTranscriptPath == "" || lr.openErr != nil {
		return core.Result{}, false
	}
	expired, block := liveDispatchVerdict(lr.ids, agentID)
	if block {
		return expired, true
	}
	res, block := ranOnVerdict(in, agentID, lr)
	if block || expired.SystemMessage == "" {
		return res, block
	}
	if res.SystemMessage != "" {
		expired.SystemMessage = res.SystemMessage + "; " + expired.SystemMessage
	}
	return expired, false
}

// liveDispatchVerdict blocks on the first live dispatch within its wait limit, else carries the first expiry note.
func liveDispatchVerdict(ids []string, agentID string) (expired core.Result, block bool) {
	for _, id := range ids {
		rec, err := loadDispatch(id)
		if err != nil || !inflight.DispatchAlive(rec.Status, rec.PID, rec.Time) || pidReused(rec) {
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

// pidReused is true when rec.PID now runs a process that started after the dispatch did; no recorded start is unknown.
func pidReused(rec agent.Record) bool {
	if rec.PIDStart == "" || rec.PID <= 0 {
		return false
	}
	got, err := proc.StartTime(rec.PID)
	return err == nil && got != rec.PIDStart
}

// leafStopVerdict blocks until the wait limit, then allows the stop with a visible note.
func leafStopVerdict(agentID string, rec agent.Record) (core.Result, bool) {
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

// leafScan is what the gates read from a leaf transcript: the ran-on and hand-back results and prior gate blocks.
type leafScan struct {
	seenBrief         bool
	ranOn, handedBack bool
	gates             int
}

// leafToolResult is one tool_result item of a user message.
type leafToolResult struct {
	Type    string          `json:"type"`
	IsError bool            `json:"is_error"`
	Content json.RawMessage `json:"content"`
}

// addLine reads one transcript line: its first user message, tool results and hook blocks; assistant text never counts.
func (s *leafScan) addLine(b []byte) {
	var m struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(b, &m) != nil || m.Type != "user" {
		return
	}
	if !s.seenBrief { // the first user message is the brief
		s.seenBrief = true
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

// ranOnVerdict blocks a gated leaf's stop or hand-back up to ranOnMaxBlocks times when no ran-on line was seen.
func ranOnVerdict(in *core.Input, agentID string, lr leafRead) (core.Result, bool) {
	t := leafAgentType(in)
	if !externalTypes[t] { // only leaves that call an external agent: codex|copilot|gemini|opencode
		return core.Result{}, false
	}
	s := lr.scan
	if lr.scanErr != nil || s.ranOn {
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
