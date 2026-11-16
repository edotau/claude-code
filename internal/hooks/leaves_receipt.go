// The receipt fix: an external-agent leaf with no `ask: ran on` line is held, run, attached or noted; never dropped.
package hooks

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
)

// editTools are the tool calls that change files: a leaf that made one did the work itself.
var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// bashWrites reports a Bash command that changes a file outside the temp dirs: a write, removal, patch or formatter.
func bashWrites(cmd string) bool {
	targets := append(core.WrittenPathsPrecise(cmd, "/"), core.RemovedPaths(cmd, "/")...)
	if slices.ContainsFunc(targets, func(p string) bool { return !scratchPath(p) }) {
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
	switch filepath.Base(toks[0]) {
	case "apply_patch", "patch":
		return true
	case "gofmt", "goimports", "shfmt":
		return slices.Contains(args, "-w")
	case "go":
		return len(args) > 0 && args[0] == "fmt"
	case "prettier":
		return slices.Contains(args, "--write") || slices.Contains(args, "-w")
	case "ruff":
		fix := len(args) > 0 && args[0] == "format" && !slices.Contains(args, "--check")
		return fix || slices.Contains(args, "--fix")
	case "black", "isort", "rustfmt":
		return !slices.Contains(args, "--check") && !slices.Contains(args, "--diff")
	}
	return false
}

// scratchPath is a path under /tmp or the OS temp dir: a relay's scratch output, not the work.
func scratchPath(p string) bool {
	return slices.ContainsFunc([]string{"/tmp", "/private/tmp", os.TempDir()}, func(dir string) bool {
		return dir != "" && strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
	})
}

// Seams for tests: the dispatch-record list and the detached launch of a brief's Run: line.
var (
	recentDispatches = agent.RecentRecords
	spawnBriefLine   = spawnDetached
)

// spawnDetached starts words (a PATH name or path, then its args) in dir, detached, output on out.
func spawnDetached(words []string, dir string, out *os.File) error {
	bin, err := exec.LookPath(words[0])
	if err != nil {
		return err
	}
	if !proc.SpawnDetached(bin, words[1:], dir, out) {
		return fmt.Errorf("%s did not start", bin)
	}
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

func loadRelayRun(agentID string) (relayRun, bool) {
	var m relayRun
	b, err := os.ReadFile(relayRunPath(agentID))
	return m, err == nil && json.Unmarshal(b, &m) == nil && !m.Started.IsZero()
}

// receiptFix: a job it started is held or attached; a never-run Run: line runs here; relay work passes with a note.
func receiptFix(in *core.Input, agentID string, lr leafRead) (core.Result, bool) {
	st := leafAgentType(in)
	m, hookRan := loadRelayRun(agentID)
	if hookRan {
		lr.scan.askTimes = append(lr.scan.askTimes, m.Started)
	}
	if recs, how, ok := leafDispatch(lr, st); ok {
		var got []receipt
		for _, rec := range recs {
			how, output := how, m.Output
			if hookRan && launchedBy(rec.Time, []time.Time{m.Started}) {
				how = "the hook ran the brief's `Run:` line itself"
			} else if len(recs) > 1 {
				output = "" // the hook's output belongs to its own launch only
			}
			r, hold, held := dispatchReceipt(rec, how, output, lr.until)
			if held {
				return hold, true
			}
			got = append(got, r)
		}
		return attachAll(in, got), false
	}
	if hookRan {
		return relayRunPending(in, m)
	}
	words := openingAskWords("Run: " + lr.scan.runLine)
	if lr.scan.edited || len(words) == 0 {
		model, _ := transcript.LastUsage(in.AgentTranscriptPath)
		return attach(in, fmt.Sprintf("NOTE: the work was done on the relay model (%s), not on %s; re-run the brief's `Run:` "+
			"line if %s must do it.", cmp.Or(model.Model, "the leaf's own model"), st, st), jobOut{}), false
	}
	return runBriefLine(in, agentID, words, lr.scan.runLine)
}

// leafDispatch is the jobs this leaf started: its printed ids' records, else the one its ask call launched.
func leafDispatch(lr leafRead, st string) (recs []agent.Record, how string, ok bool) {
	if len(lr.recs) > 0 {
		return lr.recs, "printed in its tool results", true
	}
	if len(lr.scan.askTimes) == 0 {
		return nil, "", false
	}
	owner := inflight.SessionOwner()
	for _, rec := range recentDispatches(50) {
		mine := owner == 0 || rec.Owner == 0 || rec.Owner == owner
		if mine && (rec.Agent == st || rec.Fallback == st) && launchedBy(rec.Time, lr.scan.askTimes) {
			return []agent.Record{rec}, "launched by its ask call", true
		}
	}
	return nil, "", false
}

// launchedBy is a record start within askLaunchWindow after one of the ask calls (2 s early for clock rounding).
func launchedBy(start time.Time, calls []time.Time) bool {
	return slices.ContainsFunc(calls, func(at time.Time) bool {
		return !start.Before(at.Add(-2*time.Second)) && start.Before(at.Add(askLaunchWindow))
	})
}

// receipt is one dispatch's note line and the result attached under it.
type receipt struct {
	note string
	body jobOut
}

// dispatchReceipt waits for rec; a job still running holds (up to the cap), a finished one gives its receipt.
func dispatchReceipt(rec agent.Record, how, output string, until time.Time) (receipt, core.Result, bool) {
	rec, err := awaitDispatch(rec, until)
	switch {
	case err != nil:
		return receipt{note: fmt.Sprintf("NOTE: dispatch %s (%s) became unreadable while the hand-back waited (%v); its result "+
			"is not attached (`claude-code agents result %s`).", rec.ID, how, err, rec.ID)}, core.Result{}, false
	case !jobRunning(rec):
		return endedNote(rec, how, output), core.Result{}, false
	case time.Since(rec.Time) >= leafStopMaxWait:
		return receipt{note: fmt.Sprintf("NOTE: dispatch %s (%s) still running at the %s limit; its result is not in this report "+
			"(`claude-code agents result %s`).", rec.ID, how, leafStopMaxWait, rec.ID)}, core.Result{}, false
	}
	return receipt{}, core.Result{Decision: core.Block, Reason: fmt.Sprintf("Dispatch %s (%s) is still running, so your hand-back is held, "+
		"not refused. Run `claude-code agents result -f %s` with a 600000ms Bash timeout, repeating until it finishes, then hand "+
		"back with its result and `ask: ran on` line.", rec.ID, how, rec.ID)}, true
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

// relayRunPending is the hook's launch with no dispatch record yet: its output decides, else hold while it starts.
func relayRunPending(in *core.Input, m relayRun) (core.Result, bool) {
	out := readEnds(m.Output, resultCap)
	body := jobResult(nil, m.Output)
	switch {
	case ranOnPattern().MatchString(out):
		return attach(in, "NOTE: the hook ran the brief's `Run:` line itself: "+strings.TrimSpace(ranOnLine(out)), body), false
	case didNotRunRe().MatchString(out):
		return attach(in, "NOTE: the hook ran the brief's `Run:` line itself; it ran no agent.", body), false
	case time.Since(m.Started) < askLaunchWindow:
		return core.Result{Decision: core.Block, Reason: fmt.Sprintf("The hook is running your brief's `Run:` line (`%s`, output %s), "+
			"so your hand-back is held, not refused. Run `sleep 60`, then hand back again; the hook attaches its result and "+
			"`ask: ran on` line.", m.Line, m.Output)}, true
	}
	return attach(in, fmt.Sprintf("NOTE: the hook ran the brief's `Run:` line itself, but no dispatch record or receipt "+
		"appeared within %s (output %s).", askLaunchWindow, m.Output), body), false
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

// runBriefLine starts the brief's own Run: command, unchanged and detached, then holds the hand-back for its result.
func runBriefLine(in *core.Input, agentID string, words []string, line string) (core.Result, bool) {
	fail := func(err error) (core.Result, bool) {
		return attach(in, fmt.Sprintf("NOTE: the brief's `Run:` line was never run and the hook could not start it (%v); "+
			"this report was made on the relay model.", err), jobOut{}), false
	}
	m := relayRun{Started: time.Now(), Line: line, Output: strings.TrimSuffix(relayRunPath(agentID), ".json") + ".output"}
	if err := os.MkdirAll(filepath.Dir(m.Output), 0o700); err != nil {
		return fail(err)
	}
	f, err := os.OpenFile(m.Output, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	err = spawnBriefLine(words, in.CWD, f)
	_ = f.Close()
	if err != nil {
		return fail(err)
	}
	b, _ := json.Marshal(m)
	if err := paths.AtomicWrite(relayRunPath(agentID), b, 0o600); err != nil {
		return fail(err)
	}
	return core.Result{Decision: core.Block, Reason: fmt.Sprintf("Your brief's `Run:` line was never run, so the hook started it "+
		"unchanged: `%s` (output %s). Your hand-back is held, not refused: run `sleep 60`, then hand back again; the hook "+
		"attaches its result and `ask: ran on` line.", line, m.Output)}, true
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
	res := core.Result{SystemMessage: note}
	if in.ToolName != "SubagentHandback" {
		return res
	}
	if updated := in.CloneToolInput(); updated != nil {
		msg, _ := updated["message"].(string)
		var text strings.Builder
		text.WriteString(note + "\n\n" + msg)
		for _, b := range bodies {
			text.WriteString(b.render(msg))
		}
		updated["message"] = text.String()
		res.UpdatedInput = updated
	}
	return res
}

// preserveReport saves the report a held hand-back or stop carried to the reports home and names it in the reason.
func preserveReport(in *core.Input, agentID string, lr leafRead, res core.Result) core.Result {
	text := cmp.Or(in.Nested("tool_input", "message"), in.Nested("last_assistant_message"), lr.scan.lastText)
	if strings.TrimSpace(text) == "" {
		return res
	}
	_, b, err := memory.ResolveBank("")
	if in.CWD != "" {
		b, err = memory.BankFor(in.CWD), nil
	}
	if err != nil {
		return res
	}
	path := b.Sub("reports", time.Now().Format("2006-01-02")+"-handback-"+paths.Slug(agentID, "", 40, "leaf")+".md")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil || paths.AtomicWrite(path, []byte(text), 0o600) != nil {
		return res
	}
	res.Reason += " The report you tried to hand back is saved at " + path + "."
	return res
}
