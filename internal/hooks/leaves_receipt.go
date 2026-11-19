// The receipt fix: an external-agent leaf with no `ask: ran on` line is held, run, attached or noted; never dropped.
package hooks

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// editTools are the tool calls that change files: a leaf that made one did the work itself.
var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// bashWrites reports a Bash command that changes a file outside the temp dirs: a write, removal, patch or formatter.
func bashWrites(cmd string, cwd ...string) bool {
	dir := "/"
	if len(cwd) > 0 && cwd[0] != "" {
		dir = cwd[0]
	}
	targets := append(core.WrittenPathsPrecise(cmd, dir), core.RemovedPaths(cmd, dir)...)
	if slices.ContainsFunc(targets, func(p string) bool { return !scratchPath(p, cwd...) }) {
		return true
	}
	gitWrites := shell.EachGit(cmd, "/", func(_, verb string, args []string) bool {
		return verb == "rm" || verb == "mv" || verb == "am" ||
			verb == "apply" && !slices.ContainsFunc(args, func(a string) bool { return a == "--check" || a == "--stat" || a == "--numstat" })
	})
	return gitWrites || slices.ContainsFunc(shell.Segments(cmd), func(seg string) bool {
		rest, _ := shell.Unwrap(shell.Tokens(seg), "/")
		return patchOrFormat(rest)
	})
}

// patchOrFormat is a command word that rewrites files in place: a patch tool, or a formatter told to write.
func patchOrFormat(toks []string) bool {
	if len(toks) == 0 {
		return false
	}
	args := toks[1:]
	tool := filepath.Base(toks[0])
	if tool == "apply_patch" {
		return true
	}
	printOnly := printOnlyFlags()
	flags := slices.Concat(printOnly["*"], printOnly[tool])
	if slices.ContainsFunc(args, func(a string) bool { return slices.Contains(flags, a) }) {
		return false
	}
	if patchWriteTools()[tool] {
		return true
	}
	if slices.ContainsFunc(args, func(a string) bool { return slices.Contains(formatterWriteFlags()[tool], a) }) {
		return true
	}
	return len(args) > 0 && slices.Contains(formatterWriteSubcommands()[tool], args[0])
}

var patchWriteTools = sync.OnceValue(func() map[string]bool { return shell.GuardSet("patch_write_tools") })
var formatterWriteFlags = sync.OnceValue(func() map[string][]string { return shell.GuardTools("formatter_write_flags") })
var formatterWriteSubcommands = sync.OnceValue(func() map[string][]string {
	return shell.GuardTools("formatter_write_subcommands")
})

// printOnlyFlags are the flags that make a patch tool or formatter print instead of write; "*" holds every tool's.
var printOnlyFlags = sync.OnceValue(func() map[string][]string { return shell.GuardTools("print_only_flags") })

// scratchPath is a path under /tmp, the OS temp dir or the memory reports, handoffs and notes homes: not the work.
func scratchPath(p string, cwd ...string) bool {
	if len(cwd) > 0 && cwd[0] != "" && !filepath.IsAbs(p) {
		p = filepath.Join(cwd[0], p)
	}
	p = canonicalPath(p)
	if p == "" {
		return false
	}
	dirs := []string{"/tmp", "/private/tmp", os.TempDir(), memory.SubDir("reports"), memory.SubDir("handoffs"), memory.SubDir("notes")}
	return slices.ContainsFunc(dirs, func(dir string) bool {
		if dir == "" {
			return false
		}
		rel, err := filepath.Rel(canonicalPath(dir), p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	})
}

func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	if _, err := os.Lstat(p); err == nil {
		return ""
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	real := canonicalPath(parent)
	if real == "" {
		return ""
	}
	return filepath.Join(real, filepath.Base(p))
}

// Seam for testing the detached launch of a brief's Run: line.
var spawnBriefLine = spawnDetached

// spawnDetached starts words (a PATH name or path, then its args) in dir, detached, output on out.
func spawnDetached(words []string, dir, inputPath string, out *os.File) error {
	if len(words) == 0 {
		return errors.New("empty Run line")
	}
	var input *os.File
	if inputPath != "" {
		if !filepath.IsAbs(inputPath) {
			inputPath = filepath.Join(dir, inputPath)
		}
		f, err := os.Open(inputPath)
		if err != nil {
			return err
		}
		defer f.Close()
		input = f
	}
	bin, err := exec.LookPath(words[0])
	if err != nil {
		return err
	}
	if proc.IsSelfUnderTest(bin) {
		return errors.New("refusing test binary launch")
	}
	cmd := exec.CommandContext(context.Background(), bin, words[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, out, out
	if input != nil {
		cmd.Stdin = input
	}
	cmd.Env = append(os.Environ(), agent.DispatchClaimEnv+"="+out.Name()+".claim")
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// resultCap bounds each end (head and tail) of a job result attached to a hand-back.
const resultCap = 16 << 10

// relayRun is the hook's own launch of a brief's never-run Run: line, keyed by the leaf's agent id.
type relayRun struct {
	Started time.Time `json:"started"`
	Line    string    `json:"line"`
	Output  string    `json:"output"`
}

func relayRunPath(agentID string) string {
	return filepath.Join(paths.StateDir(), "dispatch-relay", paths.Slug(agentID, "", 80, "leaf")+".json")
}

// heldPath marks a leaf held on a running job while others had finished: its later pass attaches them all.
func heldPath(agentID string) string {
	return strings.TrimSuffix(relayRunPath(agentID), ".json") + ".held"
}

func markHeld(agentID string) {
	if os.MkdirAll(filepath.Dir(heldPath(agentID)), 0o700) == nil {
		_ = os.WriteFile(heldPath(agentID), nil, 0o600)
	}
}

// wasHeld reports agentID's held marker; one older than leafStopMaxWait is stale, removed and ignored.
func wasHeld(agentID string) bool {
	fi, err := os.Stat(heldPath(agentID))
	if err == nil && time.Since(fi.ModTime()) >= leafStopMaxWait {
		_ = os.Remove(heldPath(agentID))
		return false
	}
	return err == nil
}

// claimRelayRun writes agentID's launch marker with an exclusive create; false means another hand-back holds it.
func claimRelayRun(agentID string, m relayRun) (bool, error) {
	f, err := os.OpenFile(relayRunPath(agentID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	b, _ := json.Marshal(m)
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(relayRunPath(agentID))
	}
	return err == nil, err
}

func loadRelayRun(agentID string) (relayRun, bool) {
	var m relayRun
	b, err := os.ReadFile(relayRunPath(agentID))
	return m, err == nil && json.Unmarshal(b, &m) == nil && !m.Started.IsZero()
}

// receiptFix: a job it started is held or attached; a never-run Run: line runs here; relay work passes with a note.
func receiptFix(in *core.Input, agentID string, lr *leafRead) (core.Result, bool) {
	st := relayAgent(in, lr)
	m, hookRan := loadRelayRun(agentID)
	if hookRan {
		lr.capHold(m.Started)
		if rec, ok := hookJob(*lr, m.Output); ok {
			return hookJobReceipt(in, rec, m.Output, *lr)
		}
	}
	if recs, how, ok := leafDispatch(*lr, st); ok {
		return dispatchReceipts(in, lr, recs, how)
	}
	if hookRan {
		return relayRunPending(in, *lr, m)
	}
	if len(lr.scan.askTimes) > 0 || len(lr.ids) > 0 {
		return missingIDReceipt(in, lr, st)
	}
	if _, _, err := relayInput(lr.scan.runLine); lr.scan.runLine != "" && (err != nil || len(shell.Segments(lr.scan.runLine)) != 1) {
		return attach(in, "NOTE: the hook refused unsupported shell syntax in the brief's `Run:` line; this is the relay report.", jobOut{}), false
	}
	words := briefWords(lr.scan)
	if lr.scan.edited || len(words) == 0 {
		model, _ := transcript.LastUsage(in.AgentTranscriptPath)
		return attach(in, fmt.Sprintf("NOTE: ran on the relay model (%s), not %s; re-run `Run:` if %s must do it.", cmp.Or(model.Model, "the leaf's own model"), st, st), jobOut{}), false
	}
	return runBriefLine(in, *lr, agentID, words)
}

func dispatchReceipts(in *core.Input, lr *leafRead, recs []agent.Record, how string) (core.Result, bool) {
	var got []receipt
	var held *core.Result
	for _, rec := range recs {
		r, hold := dispatchReceipt(rec, how, "", *lr)
		if hold != nil {
			held = cmp.Or(held, hold)
			continue
		}
		got = append(got, r)
	}
	if held != nil {
		lr.held = got
		return *held, true
	}
	return attachAll(in, got), false
}

// briefWords is the Run: line's words, with the brief body as the task when the line carries no prompt.
func briefWords(s leafScan) []string {
	words, input, err := relayInput(s.runLine)
	if err != nil || len(shell.Segments(s.runLine)) != 1 || askVerb(relayWords(s.runLine)) != "ask" {
		return nil
	}
	if input != "" {
		return words
	}
	if len(words) > 0 && s.task != "" && parseAskLine(context.Background(), "Run: "+s.runLine).prompt == "" {
		words = append(words, s.task)
	}
	return words
}

// unattributedNote tops a report whose ask no dispatch record could be tied to.
const unattributedNote = "NOTE: the relay's ask could not be attributed to a dispatch record, so the work may have been done by the relay model."

// missingIDReceipt binds the records askBound proves are this relay's, else holds on a running candidate, else notes.
func missingIDReceipt(in *core.Input, lr *leafRead, st string) (core.Result, bool) {
	recs := askRecords(lr.scan.askTimes, st)
	tied := slices.DeleteFunc(slices.Clone(recs), func(r agent.Record) bool { return !askBound(lr.scan.asks, r) })
	if len(tied) > 0 {
		return dispatchReceipts(in, lr, tied, "looked up by session, agent, brief and launch time")
	}
	if running := runningAfter(recs, lr.holdUntil); len(running) > 0 {
		if res, held := dispatchReceipts(in, lr, running, "a running dispatch of this agent launched after the relay's ask"); held {
			return res, true
		}
	}
	note := unattributedNote
	if len(recs) > 0 {
		ids := make([]string, len(recs))
		for i, r := range recs {
			ids[i] = r.ID
		}
		note += " Candidates (`claude-code agents result <id>`): " + strings.Join(ids, ", ") + "."
	}
	res := attach(in, note, jobOut{})
	res.Rule = "unattributed"
	if msg, ok := res.UpdatedInput["message"].(string); ok {
		res.UpdatedInput["message"] = labelUnverified(msg)
	}
	return res, false
}

func runningAfter(recs []agent.Record, holdUntil time.Time) []agent.Record {
	if holdExpired(holdUntil) {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(recs), func(r agent.Record) bool {
		return !jobRunning(r) || time.Since(r.Time) >= leafStopMaxWait
	})
}

// labelUnverified prefixes each relay-written `ask: ran on` line, keeping the text.
func labelUnverified(msg string) string {
	lines := strings.Split(msg, "\n")
	for i, l := range lines {
		if strings.Contains(l, "ask: ran on ") && !strings.HasPrefix(l, unverifiedLabel) {
			lines[i] = unverifiedLabel + l
		}
	}
	return strings.Join(lines, "\n")
}

const unverifiedLabel = "unverified (written by the relay): "

func firstAskAt(times []time.Time) time.Time {
	var first time.Time
	for _, at := range times {
		if !at.IsZero() && (first.IsZero() || at.Before(first)) {
			first = at
		}
	}
	return first
}

// askRecords is this session's records of agentName launched at or after the earliest ask, newest first.
func askRecords(times []time.Time, agentName string) []agent.Record {
	first := firstAskAt(times)
	if first.IsZero() {
		return nil
	}
	return slices.DeleteFunc(agent.RecentRecords(agent.AllRecords), func(r agent.Record) bool {
		return r.Owner == 0 || r.Owner != watchOwner() || r.Agent != agentName || r.Time.Before(first)
	})
}

func missingIDLimit(in *core.Input) (core.Result, bool) {
	return attach(in, fmt.Sprintf("NOTE: no exact dispatch id at the %s limit; failing open with the relay report.", leafStopMaxWait), jobOut{}), false
}

// leafDispatch uses only records named by this leaf's tool results.
func leafDispatch(lr leafRead, _ string) ([]agent.Record, string, bool) {
	return lr.recs, "printed in its tool results", len(lr.recs) > 0
}

// receipt is one dispatch's note line and the result attached under it.
type receipt struct {
	note string
	body jobOut
}

// dispatchReceipt waits for rec; a job still running holds (up to the cap), a finished one gives its receipt.
func dispatchReceipt(rec agent.Record, how, output string, lr leafRead) (receipt, *core.Result) {
	rec, err := awaitDispatch(rec, lr.until)
	switch {
	case err != nil:
		return receipt{note: fmt.Sprintf("NOTE: dispatch %s (%s) became unreadable while the hand-back waited (%v); its result "+
			"is not attached (`claude-code agents result %s`).", rec.ID, how, err, rec.ID)}, nil
	case !jobRunning(rec):
		return endedNote(rec, how, output), nil
	case time.Since(rec.Time) >= leafStopMaxWait || holdExpired(lr.holdUntil):
		return receipt{note: fmt.Sprintf("NOTE: dispatch %s (%s) still running at the %s limit; its result is not in this report "+
			"(`claude-code agents result %s`).", rec.ID, how, leafStopMaxWait, rec.ID)}, nil
	}
	if slices.Contains(lr.scan.heldJobs, rec.ID) {
		return receipt{}, &core.Result{Decision: core.Block, Rule: "job-running", Verdict: core.VerdictHeld, Reason: repeatHold(rec.ID)}
	}
	return receipt{}, &core.Result{Decision: core.Block, Rule: "job-running", Verdict: core.VerdictHeld, Reason: fmt.Sprintf("%s Dispatch %s (%s) is still running. %s", heldLead, rec.ID, how, pollOnce(rec.ID))}
}

// endedNote is a finished job's ran-on line and result.
func endedNote(rec agent.Record, how, output string) receipt {
	body := jobResult(&rec, output)
	if line := rec.ResultEnvelope(io.Discard).RanOn; line != "" {
		return receipt{fmt.Sprintf("NOTE: dispatch %s (%s) supplied the receipt: ask: %s", rec.ID, how, line), body}
	}
	return receipt{fmt.Sprintf("NOTE: dispatch %s (%s) ended %s without running an agent.", rec.ID, how, rec.Status), body}
}

// attachAll attaches one receipt as is; several go in dispatch order, each under its note, the latest on top.
func attachAll(in *core.Input, rs []receipt) core.Result {
	if len(rs) == 1 {
		return attach(in, rs[0].note, rs[0].body)
	}
	bodies := make([]jobOut, len(rs))
	for i, r := range rs {
		bodies[i] = jobOut{summary: strings.TrimSpace(r.note + "\n" + r.body.summary), answer: r.body.answer}
	}
	return attach(in, rs[len(rs)-1].note, bodies...)
}

// relayRunPending is the hook's launch with no record yet: its output decides, else its record, else a hold.
func relayRunPending(in *core.Input, lr leafRead, m relayRun) (core.Result, bool) {
	lr.capHold(m.Started)
	if holdExpired(lr.holdUntil) {
		return missingIDLimit(in)
	}
	out := readEnds(m.Output, resultCap)
	body := jobResult(nil, m.Output)
	switch {
	case ranOnPattern().MatchString(out):
		return attach(in, "NOTE: the hook ran the brief's `Run:` line itself: "+strings.TrimSpace(ranOnLine(out)), body), false
	case didNotRunRe().MatchString(out):
		return attach(in, "NOTE: the hook ran the brief's `Run:` line itself; it ran no agent.", body), false
	case time.Since(m.Started) < askLaunchWindow:
		if rec, ok := hookJob(lr, m.Output); ok {
			return hookJobReceipt(in, rec, m.Output, lr)
		}
		return core.Result{Decision: core.Block, Rule: "run-line-pending", Verdict: core.VerdictHeld, Reason: fmt.Sprintf("%s The hook is running your brief's `Run:` line (`%s`, output %s). %s", heldLead, m.Line, m.Output, handBackForID)}, true
	}
	return core.Result{Decision: core.Block, Rule: "no-dispatch-id", Verdict: core.VerdictRefused, Reason: fmt.Sprintf("No exact dispatch id appeared within %s. "+
		"Recover the id from the hook launch output %s, run `claude-code agents result -f <id>` once, "+
		"and hand back again; sibling records cannot supply the receipt.", askLaunchWindow, m.Output)}, true
}

// handBackForID is the hold's instruction while the hook's launch has no dispatch record to name yet.
const handBackForID = "Hand back again now, without sleeping: the hook then names its job; run `claude-code agents result " +
	"-f <id>` once, in the foreground, and hand back when it returns."

func hookDispatchID(output string) string {
	b, err := os.ReadFile(output + ".claim")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// hookJob waits for the exact ID published by this hook launch; it never searches sibling records.
func hookJob(lr leafRead, output string) (agent.Record, bool) {
	for {
		if id := hookDispatchID(output); id != "" {
			if rec, err := loadDispatch(id); err == nil {
				return rec, true
			}
		}
		if !time.Now().Add(handbackPoll).Before(lr.until) {
			return agent.Record{}, false
		}
		time.Sleep(handbackPoll)
	}
}

// hookJobReceipt is the hook launch's job receipt once it ended, else the hold naming its id.
func hookJobReceipt(in *core.Input, rec agent.Record, output string, lr leafRead) (core.Result, bool) {
	r, hold := dispatchReceipt(rec, "the hook ran the brief's `Run:` line itself", output, lr)
	if hold != nil {
		return *hold, true
	}
	return attachAll(in, []receipt{r}), false
}

// ranOnLine is the first `ask: ran on` line of out.
func ranOnLine(out string) string {
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, "ask: ran on ") {
			return l
		}
	}
	return ""
}

// runBriefLine starts the Run: command detached with any missing task, then holds for its result.
func runBriefLine(in *core.Input, lr leafRead, agentID string, words []string) (core.Result, bool) {
	fail := func(err error) (core.Result, bool) {
		return attach(in, fmt.Sprintf("NOTE: the brief's `Run:` line was never run and the hook could not start it (%v); "+
			"this report was made on the relay model.", err), jobOut{}), false
	}
	line := lr.scan.runLine
	_, input, err := relayInput(line)
	if err != nil || len(shell.Segments(line)) != 1 {
		return fail(errors.New("unsupported shell syntax"))
	}
	m := relayRun{Started: time.Now(), Line: line, Output: strings.TrimSuffix(relayRunPath(agentID), ".json") + ".output"}
	if err := os.MkdirAll(filepath.Dir(m.Output), 0o700); err != nil {
		return fail(err)
	}
	if claimed, err := claimRelayRun(agentID, m); err != nil {
		return fail(err)
	} else if !claimed { // an overlapping hand-back launched it: hold on that launch, never start a second
		if cur, ok := loadRelayRun(agentID); ok {
			m = cur
		} else if fi, err := os.Stat(relayRunPath(agentID)); err == nil {
			m.Started = fi.ModTime() // a marker still being written, or unreadable: its age bounds the hold
		}
		return relayRunPending(in, lr, m)
	}
	f, err := os.OpenFile(m.Output, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err == nil {
		err = spawnBriefLine(words, in.CWD, input, f)
		_ = f.Close()
	}
	if err != nil {
		_ = os.Remove(relayRunPath(agentID)) // nothing started: a later hand-back may try again
		return fail(err)
	}
	lr.capHold(m.Started)
	if rec, ok := hookJob(lr, m.Output); ok {
		return hookJobReceipt(in, rec, m.Output, lr)
	}
	return core.Result{Decision: core.Block, Rule: "run-line-started", Verdict: core.VerdictHeld, Reason: fmt.Sprintf("%s Your brief's `Run:` line was never run, so the hook started it "+
		"with your brief as its task: `%s` (output %s). %s", heldLead, line, m.Output, handBackForID)}, true
}

// jobOut is the job result a hand-back carries: one summary line and the job's final answer.
type jobOut struct{ summary, answer string }

// tailLines is how many non-noise log lines stand in for a job that left no final answer.
const tailLines = 20

// jobResult is rec's (nil: none) final answer and summary: its last message, else the answer in output or the log.
func jobResult(rec *agent.Record, output string) jobOut {
	text := ""
	if output != "" {
		text = readEnds(output, resultCap)
	}
	var env wire.Envelope
	if rec != nil {
		if strings.TrimSpace(text) == "" {
			text = readEnds(rec.LogPath(), resultCap)
		}
		env, _ = rec.SavedEnvelope()
	}
	lines := slices.DeleteFunc(strings.Split(text, "\n"), func(l string) bool { return strings.TrimSpace(l) == "" })
	j := jobOut{summary: jobSummary(rec, env, lines)}
	if rec != nil {
		j.answer = lastMessage(rec.EventsPath())
	}
	j.answer = cmp.Or(j.answer, strings.TrimSpace(env.Answer), plainAnswer(lines), noiseFreeTail(lines))
	if j.answer == "" && rec != nil {
		j.answer = "(no log; `claude-code agents result " + rec.ID + "`)"
	}
	return j
}

// jobSummary is `<agent> <status> · N steps · <duration> · tokens in=.. out=..`, each part only when known.
func jobSummary(rec *agent.Record, env wire.Envelope, lines []string) string {
	var parts []string
	if rec != nil {
		parts = append(parts, strings.TrimSpace(rec.Agent+" "+rec.Status))
	}
	if n := len(slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return !strings.HasPrefix(l, stepLine) })); n > 0 {
		parts = append(parts, fmt.Sprintf("%d steps", n))
	}
	if rec != nil && rec.Elapsed() != "" {
		parts = append(parts, rec.Elapsed())
	}
	if u := env.Usage; u != nil {
		parts = append(parts, fmt.Sprintf("tokens in=%d out=%d", u.InputTokens+u.CacheCreationTokens+u.CacheReadTokens, u.OutputTokens))
	}
	return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), " · ")
}

// stepLine starts a job log's per-step line; heartbeatLine its still-running line.
const (
	stepLine      = "▸ result: step finished"
	heartbeatLine = "▸ heartbeat: "
)

// lastMessage is the text of the last turn in an events file (from its last `sep` text event); "" when none.
func lastMessage(eventsPath string) string {
	f, err := os.Open(eventsPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	var msg strings.Builder
	_ = textutil.EachLineErr(f, func(b []byte) {
		var ev wire.Event
		if json.Unmarshal(b, &ev) != nil || ev.Type != wire.EventText {
			return
		}
		if ev.Sep {
			msg.Reset()
		}
		msg.WriteString(ev.Text)
	})
	return strings.TrimSpace(msg.String())
}

// plainAnswer is the log's plain (non-`▸`) lines without the trailing ask/session/resume footers.
func plainAnswer(lines []string) string {
	plain := slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return strings.HasPrefix(l, "▸ ") })
	for len(plain) > 0 && slices.ContainsFunc([]string{"ask: ", "session: ", "resume: "}, func(p string) bool {
		return strings.HasPrefix(plain[len(plain)-1], p)
	}) {
		plain = plain[:len(plain)-1]
	}
	return strings.TrimSpace(strings.Join(plain, "\n"))
}

// noiseFreeTail is the last tailLines lines that are neither heartbeat nor per-step lines.
func noiseFreeTail(lines []string) string {
	kept := slices.DeleteFunc(slices.Clone(lines), func(l string) bool {
		return strings.HasPrefix(l, heartbeatLine) || strings.HasPrefix(l, stepLine)
	})
	return strings.Join(kept[max(0, len(kept)-tailLines):], "\n")
}

// render is the attached section; an answer the report already quotes verbatim is named, not repeated.
func (j jobOut) render(report string) string {
	answer := j.answer
	if answer != "" && strings.Contains(strings.Join(strings.Fields(report), " "), strings.Join(strings.Fields(answer), " ")) {
		answer = "(final answer already in the report above)"
	}
	text := strings.TrimSpace(j.summary + "\n\n" + answer)
	if text == "" {
		return ""
	}
	return "\n\n--- job result ---\n" + text
}

// attach puts a one-line note on top of the hand-back and the results below it; a stop gets the note only.
func attach(in *core.Input, note string, bodies ...jobOut) core.Result {
	res := core.Result{SystemMessage: note, Rule: "receipt", Verdict: core.VerdictPassed}
	if in.ToolName != "SubagentHandback" {
		return res
	}
	if updated := in.CloneToolInput(); updated != nil {
		msg, _ := updated["message"].(string)
		var text strings.Builder
		text.WriteString(note)
		text.WriteString("\n\n")
		text.WriteString(msg)
		for _, b := range bodies {
			text.WriteString(b.render(msg))
		}
		updated["message"] = text.String()
		res.UpdatedInput = updated
	}
	return res
}

// preserveReport saves a held hand-back's or stop's report and finished answers, naming the file in the reason.
func preserveReport(in *core.Input, agentID string, lr leafRead, res core.Result) core.Result {
	if len(lr.held) > 0 {
		markHeld(agentID)
	}
	text := cmp.Or(in.Nested("tool_input", "message"), in.Nested("last_assistant_message"), lr.scan.lastText)
	var sb strings.Builder
	sb.WriteString(text)
	for _, r := range lr.held { // the finished jobs' answers, so a leaf that stops on this hold loses none
		fmt.Fprintf(&sb, "\n\n%s%s", r.note, r.body.render(text))
	}
	text = sb.String()
	if strings.TrimSpace(text) == "" {
		return res
	}
	text = redact.Text(text)
	path := memory.Sub("reports", time.Now().Format("2006-01-02")+"-handback-"+paths.Slug(agentID, "", 40, "leaf")+".md")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil || paths.AtomicWrite(path, []byte(text), 0o600) != nil {
		return res
	}
	if !strings.Contains(res.Reason, heldAgain) {
		res.Reason += " The report you tried to hand back is saved at " + path + "."
	}
	return res
}
