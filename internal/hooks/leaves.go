// The dispatch group's PreToolUse runner: cap → pin → claim on one Agent/Task/Workflow spawn, so a blocked
// dispatch claims nothing; the post-side runners (Release, Stop) follow Effort below.
package hooks

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

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

// lowerFirst lowercases desc's first letter unless its first word looks like an acronym or identifier.
func lowerFirst(desc string) string {
	word := strings.TrimRight(strings.Fields(desc + " x")[0], ".,:;!?")
	if strings.ContainsAny(word, "._/`0123456789") {
		return desc
	}
	r := []rune(desc)
	i := 0
	for i < len(r) && unicode.IsSpace(r[i]) {
		i++
	}
	if i >= len(r) || !unicode.IsUpper(r[i]) || (i+1 < len(r) && unicode.IsUpper(r[i+1])) {
		return desc
	}
	r[i] = unicode.ToLower(r[i])
	return string(r)
}

// Effort: the cap check, then the pin step, then the claim (a Workflow claims one slot per agent() site).
func Effort(in *core.Input) core.Result {
	if !core.SpawnTools[in.ToolName] && in.ToolName != "Workflow" {
		return core.Result{}
	}
	ctx := context.Background() // the hook entry: no caller context
	owner := inflight.SessionOwner()
	if owner == 0 {
		return agentEffortResult(ctx, in) // an unidentifiable owner claims nothing: fail open
	}
	account, lim := router.SessionAccountContext(ctx), router.CurrentLimits()
	claim := inflight.Claim{Owner: owner, ID: in.Nested("tool_use_id"), Account: account, N: 1, Transcript: in.TranscriptPath}
	if ok, snap := inflight.Fits(claim, lim); !ok {
		return blockAtCap(snap, account, lim)
	}
	res := agentEffortResult(ctx, in)
	if res.Decision == core.Block || claim.ID == "" { // no tool_use_id: no release could match the claim, so make none
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

// workflowLeaves counts agent() call sites in the script the Workflow runs: the pin step's rewrite, else the original.
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

// Release side of the cap: PostToolUseFailure, PostToolUse(Workflow), SubagentStop retire the dispatch-effort markers.

// Release retires the exact tool_use_id a spawn or Workflow claimed; a blocked dispatch wrote none, so it is a no-op.
func Release(in *core.Input) core.Result {
	if owner, id := inflight.SessionOwner(), in.Nested("tool_use_id"); owner != 0 && id != "" {
		inflight.Release(owner, id)
		if core.SpawnTools[in.ToolName] { // only a spawn claims a plan task; a failed one never reaches SubagentStop
			return core.WithStderr(core.Result{}, docs.ClearPlanClaim(in.CWD, id))
		}
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
	lr := readLeaf(in.AgentTranscriptPath, leafWants(in), in.CWD)
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

// The leaf-stop gate: no stop while its dispatch runs; an external-agent leaf needs a ran-on line in a tool result.

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
	return leafWant{ids: in.Agent() == core.AgentClaude, brief: true, files: true}
}

// leafRead is one pass over a leaf transcript: its dispatch ids, brief scan and background output files.
type leafRead struct {
	ids, files []string
	recs       []agent.Record // ids' readable records, loaded once by leafGate and shared by its gates
	held       []receipt      // the finished jobs' receipts at a hold on a still-running one
	scan       leafScan
	openErr    error     // the transcript could not be opened
	scanErr    error     // reading stopped early: scan is partial and files are dropped
	until      time.Time // Handback's deadline for waiting on a running job; zero never waits
}

// readLeaf reads the transcript once; an empty path reads nothing and reports an open error.
func readLeaf(path string, w leafWant, cwd ...string) (r leafRead) {
	if len(cwd) > 0 {
		r.scan.cwd = cwd[0]
	}
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
	seenFiles := map[string]bool{}
	r.scanErr = textutil.EachLineErr(f, func(b []byte) {
		if w.files {
			r.files = appendOutputFiles(r.files, seenFiles, b)
		}
		if w.brief || w.ids && (bytes.Contains(b, toolUseKey) || bytes.Contains(b, toolResultKey)) {
			r.scan.addLine(b)
		}
	})
	if r.scanErr != nil {
		r.files = nil // an unreadable transcript reaps nothing
	}
	if w.ids {
		for _, p := range r.scan.bgAsks {
			r.scan.addIDs(readEnds(p, savedResultCap))
		}
		r.ids = r.scan.ids
	}
	return r
}

// leafStopBlock is leafGate over a fresh read of the leaf's transcript.
func leafStopBlock(in *core.Input, agentID string) (core.Result, bool) {
	return leafGate(in, agentID, readLeaf(in.AgentTranscriptPath, leafWants(in), in.CWD))
}

// leafGate returns a Block while a dispatch the stopping leaf started is alive; any error fails open.
func leafGate(in *core.Input, agentID string, lr leafRead) (core.Result, bool) {
	if in.Agent() != core.AgentClaude || in.AgentTranscriptPath == "" || lr.openErr != nil {
		return core.Result{}, false
	}
	lr.recs = loadRecords(lr.ids)
	expired, block := liveDispatchVerdict(in, &lr, agentID)
	if block {
		return preserveReport(in, agentID, lr, expired), true
	}
	res, block := ranOnVerdict(in, agentID, &lr)
	if block {
		return preserveReport(in, agentID, lr, res), true
	}
	_ = os.Remove(heldPath(agentID)) // the hand-back or stop passes: its hold is over
	return mergeGate(expired, res), false
}

// mergeGate joins the dispatch note and the receipt verdict; the receipt's attached message wins.
func mergeGate(expired, res core.Result) core.Result {
	if expired.SystemMessage == "" || (res.UpdatedInput != nil && expired.UpdatedInput != nil) {
		return res
	}
	if res.SystemMessage != "" {
		expired.SystemMessage = res.SystemMessage + "; " + expired.SystemMessage
	}
	if res.UpdatedInput != nil {
		expired.UpdatedInput = res.UpdatedInput
	}
	return expired
}

// loadRecords is the readable dispatch records of ids, in dispatch order: the one selection both gates share.
func loadRecords(ids []string) []agent.Record {
	var recs []agent.Record
	for _, id := range ids {
		if rec, err := loadDispatch(id); err == nil {
			recs = append(recs, rec)
		}
	}
	return recs
}

// liveDispatchVerdict waits on each live dispatch, updating lr.recs; it blocks on one still running, else notes.
func liveDispatchVerdict(in *core.Input, lr *leafRead, agentID string) (expired core.Result, block bool) {
	recs, waited := slices.Clone(lr.recs), map[int]bool{}
	for i, rec := range recs {
		if !jobRunning(rec) {
			continue
		}
		rec, err := awaitDispatch(rec, lr.until)
		if err != nil {
			continue
		}
		recs[i] = rec
		if !jobRunning(rec) {
			waited[i] = true
			continue
		}
		res, block := leafStopVerdict(agentID, rec)
		if block { // keep the finished answers: the hold saves them and the later pass attaches them
			lr.recs, lr.held = recs, endedReceipts(recs, waited)
			return res, true
		}
		if expired.SystemMessage == "" {
			expired = res
		}
	}
	lr.recs = recs
	if len(waited) > 0 || wasHeld(agentID) { // only then are the receipts used: building them reads every job's log
		if ended := endedReceipts(recs, waited); len(ended) > 0 {
			expired = attachAll(in, ended) // a finished job's answer outranks a limit note
		}
	}
	return expired, false
}

// endedReceipts is every finished record's receipt in dispatch order, so an earlier finished job is not dropped.
func endedReceipts(recs []agent.Record, waited map[int]bool) []receipt {
	var ended []receipt
	for i, rec := range recs {
		switch {
		case jobRunning(rec):
		case waited[i]:
			ended = append(ended, endedNote(rec, "ended while the hand-back waited", ""))
		default:
			ended = append(ended, endedNote(rec, "finished before the hand-back", ""))
		}
	}
	return ended
}

// jobRunning is a live dispatch whose pid still belongs to it.
func jobRunning(rec agent.Record) bool { return rec.Alive() && !pidReused(rec) }

// Handback's wait seams: the poll interval and the budget, its spec's less two HookMargins for the read and render.
var (
	handbackPoll = 250 * time.Millisecond
	handbackWait = func() time.Duration { s, _ := core.SpecOf("dispatch-handback"); return s.Budget - 2*core.HookMargin }()
)

// awaitDispatch re-reads a running rec until it ends or until passes; an unreadable record returns the error.
func awaitDispatch(rec agent.Record, until time.Time) (agent.Record, error) {
	for jobRunning(rec) && time.Since(rec.Time) < leafStopMaxWait && time.Now().Add(handbackPoll).Before(until) {
		time.Sleep(handbackPoll)
		next, err := loadDispatch(rec.ID)
		if err != nil {
			return rec, err
		}
		rec = next
	}
	return rec, nil
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
	return core.Result{Decision: core.Block, Reason: fmt.Sprintf("Dispatch %s you started is still running. %s", rec.ID, pollOnce(rec.ID))}, true
}

// pollOnce is the relay contract's wait rule, quoted in every hold on a running job.
func pollOnce(id string) string {
	return fmt.Sprintf("Run `claude-code agents result -f %s` once, in the foreground (600000ms Bash timeout); rerun it only "+
		"if it times out with the job still running, at most 12 times. The moment it returns with the job ended, hand back "+
		"its result.", id)
}

// ranOnPattern matches the `ask: ran on <agent> · ` stderr line or the json "ran_on" field; docs quoting it never.
var ranOnPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`ask: ran on [\w.-]+ · |"ran_on":\s*"ran on [\w.-]+ · `)
})

// ownIDRe matches a whole line naming an ask's own job: the `--background` launch line, the exit line, a bare id.
var ownIDRe = sync.OnceValue(func() *regexp.Regexp {
	const id = `(\d{8}T\d{6}\.\d{3}-[A-Za-z0-9._-]+)`
	return regexp.MustCompile(`^(?:job ` + id + ` started|ask: dispatch ` + id + `|` + id + `$)`)
})

// ansiRe matches a terminal colour or cursor sequence.
var ansiRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`) })

// ownIDs is the ids an ask's output prints of its own job: on its head line and its exit line, never in the answer.
func ownIDs(text string) []string {
	var lines []string
	for l := range strings.SplitSeq(ansiRe().ReplaceAllString(text, ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	var ids []string
	if i := slices.IndexFunc(lines, func(l string) bool { return !strings.HasPrefix(l, "ask: ") }); i >= 0 {
		ids = append(ids, headID(lines[i]))
	}
	return append(ids, exitID(lines))
}

// headID is the id on an ask's first output line: the `job <id> started` launch line or the json envelope's own id.
func headID(line string) string {
	if m := ownIDRe().FindStringSubmatch(line); m != nil {
		return cmp.Or(m[1], m[2], m[3])
	}
	var env struct {
		ID         string `json:"id"`
		DispatchID string `json:"dispatch_id"`
	}
	if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &env) != nil {
		return ""
	}
	if env.DispatchID != "" {
		env.ID = env.DispatchID
	}
	if m := ownIDRe().FindStringSubmatch(env.ID); m != nil {
		return m[3]
	}
	return ""
}

// exitID is the last `ask: dispatch <id>` line past trailing footer lines (incl. a background `[exited…]`).
func exitID(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if i > 0 && lines[i-1] == "ask: dispatch" {
			l, i = lines[i-1]+" "+l, i-1
		}
		if m := ownIDRe().FindStringSubmatch(l); m != nil && m[2] != "" {
			return m[2]
		}
		if !slices.ContainsFunc([]string{"ask: ", "session: ", "resume: ", "claude-code: ", "[exited with code "}, func(p string) bool { return strings.HasPrefix(l, p) }) {
			return ""
		}
	}
	return ""
}

// toolUseKey and toolResultKey prefilter the lines an ids-only read parses; an escaped quote in prose never matches.
var toolUseKey, toolResultKey = []byte(`"tool_use"`), []byte(`"tool_result"`)

// askLaunchWindow is how long after a leaf's ask call its dispatch record may start; a later one is another job.
const askLaunchWindow = 2 * time.Minute

// handedBackMark is SubagentHandback's success result: the run is over, so a later stop block cannot reopen it.
const handedBackMark = "Report delivered to your caller"

// didNotRunRe matches the lines an ask prints when no agent ran: its `did not run` line or a pre-run refusal.
var didNotRunRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^(?:ask: did not run |claude-code: ask: )`)
})

// leafScan is what the gates read from a leaf transcript: ask calls and their results, the hand-back, prior blocks.
type leafScan struct {
	cwd               string
	pendingAsks       map[string]time.Time
	pendingEdits      map[string]bool
	seenBrief         bool
	runLine           string          // the brief's opening `Run:` command, the one the hook runs
	task              string          // the brief after its Run: line: the prompt a promptless Run: line is missing
	askTimes          []time.Time     // when each launching (ask|dispatch) call was made, to find its dispatch record
	askCalls          map[string]bool // tool_use ids of Bash calls that ran an ask, dispatch or result
	ids               []string        // dispatch ids those calls' own results printed, in order: the leaf's jobs
	ranOn, handedBack bool
	askFailed         bool // an ask call's result says no agent ran
	resumed           bool
	edited            bool   // the leaf edited files (an edit tool or a file-changing Bash call): it did the work itself
	lastText          string // the leaf's last assistant text, its report at a stop
	bgAsks            []string
	polls             map[string]pollCall // tool_use id → each `agents result <id>` call
	reported          []string            // ids already known at the last delivered hand-back
}

// leafToolResult is one tool_result item of a user message.
type leafToolResult struct {
	Type      string          `json:"type"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// leafToolUse is one tool_use item of an assistant message; only a Bash command is read.
type leafToolUse struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
	} `json:"input"`
}

// addLine reads one transcript line: the brief, tool calls and results, hook blocks; assistant text never counts.
func (s *leafScan) addLine(b []byte) {
	var m struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Message   struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	switch {
	case m.Type == "assistant":
		s.addCalls(m.Message.Content, m.Timestamp)
		if text := transcript.ContentText(m.Message.Content); strings.TrimSpace(text) != "" {
			s.lastText = text
		}
	case m.Type != "user":
	case !s.seenBrief && !bytes.Contains(b, toolResultKey): // the first user message is the brief
		s.seenBrief = true
		if first, rest, _ := strings.Cut(strings.TrimSpace(transcript.ContentText(m.Message.Content)), "\n"); strings.HasPrefix(first, "Run:") {
			s.runLine, s.task = strings.TrimSpace(strings.TrimPrefix(first, "Run:")), strings.TrimSpace(rest)
		}
	default:
		s.addResults(m.Message.Content)
	}
}

func (s *leafScan) addBgAsk(text string) {
	for _, m := range bgOutputPattern().FindAllStringSubmatch(text, -1) {
		if !slices.Contains(s.bgAsks, m[1]) {
			s.bgAsks = append(s.bgAsks, m[1])
		}
	}
}

// addIDs adds the not yet seen dispatch ids an ask's output prints of its own job.
func (s *leafScan) addIDs(text string) {
	for _, id := range ownIDs(text) {
		if id != "" && !slices.Contains(s.ids, id) {
			s.ids = append(s.ids, id)
		}
	}
}

// askOutputRan reads each background ask's output for a receipt or a did-not-run line.
func askOutputRan(paths []string) (ran, failed bool) {
	for _, p := range paths {
		text := readEnds(p, savedResultCap)
		ok := ranOnPattern().MatchString(text) && boundReceipt(text)
		ran = ran || ok
		failed = failed || !ok && didNotRunRe().MatchString(text)
	}
	return ran, failed
}

// addCalls records the ids (and time, when known) of the Bash calls in an assistant message that run an ask or result.
func (s *leafScan) addCalls(content json.RawMessage, at time.Time) {
	var items []leafToolUse
	if json.Unmarshal(content, &items) != nil {
		return
	}
	for _, it := range items {
		if it.Type != "tool_use" {
			continue
		}
		if editTools[it.Name] {
			s.trackEdit(it.ID, !scratchPath(it.Input.FilePath, s.cwd))
			continue
		}
		if it.Name != "Bash" {
			continue
		}
		calls := relayCommands(it.Input.Command)
		calls = slices.DeleteFunc(calls, func(words []string) bool { return askVerb(words) == "" })
		if len(calls) == 0 {
			s.trackEdit(it.ID, bashWrites(it.Input.Command, s.cwd))
			if job, _, ok := verbJob(it.Input.Command, "wait"); ok && it.ID != "" {
				s.addPoll(it.ID, job, false)
			}
			continue
		}
		if it.ID == "" {
			continue
		}
		s.recordAsk(it, calls, at)
	}
}

// addResults reads a user message's tool results, or a plain-text turn's Stop/SubagentStop hook feedback.
func (s *leafScan) addResults(content json.RawMessage) {
	var items []leafToolResult
	if json.Unmarshal(content, &items) != nil {
		if s.handedBack && !strings.Contains(transcript.ContentText(content), "hook feedback:") {
			s.resumed = true
		}
		return
	}
	for _, it := range items {
		if it.Type == "tool_result" {
			s.scanResult(it)
		}
	}
}

// scanResult reads one tool result: an ask call's ran-on or did-not-run line, a delivered or denied hand-back.
func (s *leafScan) scanResult(it leafToolResult) {
	raw := transcript.ContentText(it.Content)
	text := resultText(raw)
	success := !it.IsError && !hookDenyRe().MatchString(text)
	if success && s.pendingEdits[it.ToolUseID] {
		s.edited = true
	}
	if at, ok := s.pendingAsks[it.ToolUseID]; ok {
		if success {
			s.askTimes = append(s.askTimes, at)
		}
		delete(s.pendingAsks, it.ToolUseID)
	}
	if s.askCalls[it.ToolUseID] && success {
		s.addBgAsk(text)
		s.addIDs(strings.TrimPrefix(text, raw+"\n")) // a persisted result's ids sit in its saved output, not the preview
		ran := ranOnPattern().MatchString(text) && boundReceipt(text)
		s.ranOn = s.ranOn || ran
		s.askFailed = s.askFailed || !ran && didNotRunRe().MatchString(text)
	}
	if p, ok := s.polls[it.ToolUseID]; ok {
		if m := bgOutputPattern().FindStringSubmatch(text); m != nil {
			p.bgFile = m[1]
		} else {
			p.done = true
			if !it.IsError {
				s.addPollID(p.job)
			}
		}
		s.polls[it.ToolUseID] = p
	}
	if it.IsError || !strings.Contains(text, handedBackMark) {
		return
	}
	s.handedBack, s.reported = true, slices.Clone(s.ids)
}

// savedResultRe matches a persisted tool result's "Full output saved to:" path; only a tool-results file is read.
var savedResultRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^Output too large \([^)]*\)\. Full output saved to: (/\S*/tool-results/[^\s/]+)$`)
})

// savedResultCap bounds each end (head and tail) read from a persisted tool result.
const savedResultCap = 64 << 10

// resultText is a tool result's text; a persisted result (2 KB preview) gains the head and tail of its saved output.
func resultText(text string) string {
	m := savedResultRe().FindStringSubmatch(text)
	if m == nil {
		return text
	}
	saved := readEnds(m[1], savedResultCap)
	if saved == "" {
		return text
	}
	return text + "\n" + saved
}

// readEnds is the first and last n bytes of the file, all of it when it fits in 2n; "" when it cannot be read.
func readEnds(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	size := fi.Size()
	if size <= 2*n {
		b, _ := io.ReadAll(io.NewSectionReader(f, 0, size))
		return string(b)
	}
	head, _ := io.ReadAll(io.NewSectionReader(f, 0, n))
	tail, _ := io.ReadAll(io.NewSectionReader(f, size-n, n))
	return string(head) + "\n" + string(tail)
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

// isRelay is a leaf of an external type, or any leaf whose brief opens with a `Run: claude-code agents ask` line.
func isRelay(in *core.Input, lr *leafRead) bool {
	return externalTypes[leafAgentType(in)] || len(openingAskWords("Run: "+lr.scan.runLine)) > 0
}

// relayAgent uses the external leaf type, else the Run: line's --agent (default claude).
func relayAgent(in *core.Input, lr *leafRead) string {
	if st := leafAgentType(in); externalTypes[st] {
		return st
	}
	return cmp.Or(parseAskLine(context.Background(), "Run: "+lr.scan.runLine).agent, agentid.Claude)
}

// labelAgent is true for an agent a dispatch label may name (claude or an external type), and a type that carries one.
func labelAgent(name string) bool { return name == agentid.Claude || externalTypes[name] }

// ranOnVerdict fixes a gated leaf's missing ran-on line: it holds, runs, attaches or notes, and never drops the report.
func ranOnVerdict(in *core.Input, agentID string, lr *leafRead) (core.Result, bool) {
	// only relay leaves (an external type, or a brief opening with a Run: ask line) that show no ran-on receipt
	if !isRelay(in, lr) || lr.scanErr != nil || lr.scan.ranOn {
		return core.Result{}, false
	}
	ran, failed := askOutputRan(lr.scan.bgAsks)
	if ran {
		return core.Result{}, false
	}
	lr.scan.askFailed = lr.scan.askFailed || failed
	if lr.scan.askFailed { // the ask ran and refused or found no agent: the failure is the report, not a reason to re-ask
		return core.Result{SystemMessage: fmt.Sprintf("leaf %s: its ask did not run an agent", agentID)}, false
	}
	switch {
	case lr.scan.resumed:
		return core.Result{SystemMessage: fmt.Sprintf("leaf %s: resumed after its hand-back; no new `ask: ran on` receipt this round", agentID)}, false
	case lr.scan.handedBack: // the delivered hand-back already carried the fix
		return core.Result{}, false
	}
	return receiptFix(in, agentID, lr)
}

// Handback runs the stop gates on SubagentHandback (it ends a background leaf before SubagentStop), pollGate on Bash.
func Handback(in *core.Input) core.Result {
	if in.ToolName == "Bash" {
		return pollGate(in)
	}
	id := in.SubagentID()
	if id == "" || in.ToolName != "SubagentHandback" {
		return core.Result{}
	}
	leaf := *in
	leaf.AgentTranscriptPath = leafTranscript(in, id)
	lr := readLeaf(leaf.AgentTranscriptPath, leafWants(&leaf), leaf.CWD)
	lr.until = time.Now().Add(handbackWait)
	res, _ := leafGate(&leaf, id, lr)
	return res
}

// The leaf-stop reaper: after a stop the gate lets through, kill the leaf's own leftover background shells.

// bgOutputPattern matches both background notices; Monitor results name no output path.
var bgOutputPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`Command (?:running in background with ID: [A-Za-z0-9]+\.|was moved to the background \(ID: [A-Za-z0-9]+\)[^"]*?\.) Output is being written to: (/[^\s"]+?/tasks/[A-Za-z0-9]+\.output)\.`)
})

const reapGrace = 2 * time.Second

// Seams for tests: the process finder and the sleep; the group probe/killer live in leaves_{unix,other}.go.
var (
	reapHolders = outputHolders
	reapSleep   = time.Sleep
)

// appendOutputFiles adds the task output files one transcript line's Bash tool results named that seen lacks.
func appendOutputFiles(files []string, seen map[string]bool, b []byte) []string {
	if !bytes.Contains(b, []byte(`"tool_result"`)) {
		return files
	}
	for _, m := range bgOutputPattern().FindAllSubmatch(b, -1) {
		if f := string(m[1]); !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}
	return files
}

// outputHolders returns the pids whose stdout or stderr is one of the files, in a single scan; every error yields none.
func outputHolders(files []string) []int {
	if len(files) == 0 {
		return nil
	}
	if runtime.GOOS != "linux" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		args := append([]string{"-a", "-d", "1,2", "-t", "--"}, files...) // stdout/stderr only, like /proc
		out, err := exec.CommandContext(ctx, "lsof", args...).Output()
		if err != nil && len(out) == 0 {
			return nil
		}
		return parsePids(out)
	}
	want := map[string]bool{}
	for _, f := range files {
		if real, err := filepath.EvalSymlinks(f); err == nil {
			f = real // /proc names the resolved path, so a symlinked $TMPDIR still matches
		}
		want[f] = true
	}
	var pids []int
	for _, fd := range []string{"1", "2"} {
		links, _ := filepath.Glob("/proc/[0-9]*/fd/" + fd)
		for _, l := range links {
			if dst, err := os.Readlink(l); err == nil && want[strings.TrimSuffix(dst, " (deleted)")] {
				if pid, err := strconv.Atoi(strings.Split(l, "/")[2]); err == nil {
					pids = append(pids, pid)
				}
			}
		}
	}
	return pids
}

func parsePids(out []byte) []int {
	var pids []int
	for f := range bytes.FieldsSeq(out) {
		if pid, err := strconv.Atoi(string(f)); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// reapGroups maps holder pids to distinct process groups, never our own or the parent's (the Claude session's).
func reapGroups(pids []int) []int {
	protected := map[int]bool{reapOwnGroup(): true}
	if g, err := reapGroupOf(os.Getppid()); err == nil {
		protected[g] = true
	}
	var groups []int
	seen := map[int]bool{}
	for _, pid := range pids {
		g, err := reapGroupOf(pid)
		if err != nil || g <= 1 || pid == os.Getpid() || protected[g] || seen[g] {
			continue
		}
		seen[g] = true
		groups = append(groups, g)
	}
	return groups
}

// terminateGroups sends SIGTERM, then SIGKILL after the grace period to any group still alive.
func terminateGroups(groups []int) {
	for _, g := range groups {
		_ = reapSignal(g, syscall.SIGTERM)
	}
	for waited := time.Duration(0); waited < reapGrace; waited += 100 * time.Millisecond {
		if !anyGroupAlive(groups) {
			return
		}
		reapSleep(100 * time.Millisecond)
	}
	for _, g := range groups {
		_ = reapSignal(g, syscall.SIGKILL)
	}
}

func anyGroupAlive(groups []int) bool {
	for _, g := range groups {
		if reapSignal(g, 0) == nil {
			return true
		}
	}
	return false
}

// reapLeafBackground kills the process groups writing to the task outputs of the stopping leaf's own transcript.
func reapLeafBackground(in *core.Input, lr leafRead) {
	if in.AgentTranscriptPath == "" {
		return
	}
	if groups := reapGroups(reapHolders(lr.files)); len(groups) > 0 {
		terminateGroups(groups)
		fmt.Fprintf(os.Stderr, "dispatch-stop: reaped %d leftover background group(s)\n", len(groups))
	}
}

func (s *leafScan) trackEdit(id string, edited bool) {
	if s.pendingEdits == nil {
		s.pendingEdits = map[string]bool{}
	}
	s.pendingEdits[id] = edited
}

func boundReceipt(text string) bool {
	for _, id := range ownIDs(text) {
		if _, err := loadDispatch(id); err == nil {
			return true
		}
	}
	return false
}

func (s *leafScan) recordAsk(it leafToolUse, calls [][]string, at time.Time) {
	if s.askCalls == nil {
		s.askCalls = map[string]bool{}
	}
	s.askCalls[it.ID] = true
	if job, follow, ok := pollJob(it.Input.Command); ok {
		s.addPoll(it.ID, job, follow)
	}
	if slices.ContainsFunc(calls, func(words []string) bool { return askVerb(words) != "result" }) {
		if s.pendingAsks == nil {
			s.pendingAsks = map[string]time.Time{}
		}
		s.pendingAsks[it.ID] = at
	}
}

func (s *leafScan) addPoll(id, job string, follow bool) {
	if s.polls == nil {
		s.polls = map[string]pollCall{}
	}
	s.polls[id] = pollCall{job: job, follow: follow}
}

// addPollID binds a job this leaf's successful poll named, only when its record is this session's and one of its asks'.
func (s *leafScan) addPollID(job string) {
	rec, err := loadDispatch(job)
	if err != nil || slices.Contains(s.ids, job) || rec.Owner == 0 || rec.Owner != inflight.SessionOwner() {
		return
	}
	if slices.ContainsFunc(s.askTimes, func(at time.Time) bool {
		return !at.IsZero() && !rec.Time.Before(at) && rec.Time.Sub(at) < askLaunchWindow
	}) {
		s.ids = append(s.ids, job)
	}
}

var hookDenyRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)PreToolUse[^\n]*(?:hook error|denied|blocked)|"permissionDecision"\s*:\s*"deny"`)
})
