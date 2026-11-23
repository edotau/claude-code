// The receipt fix: an external-agent leaf with no `ask: ran on` line is held, attached or noted; never dropped.
package hooks

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// resultCap bounds each end (head and tail) of a job result attached to a hand-back.
const resultCap = 16 << 10

// heldPath marks a leaf held on a running job while others had finished: its later pass attaches them all.
func heldPath(agentID string) string {
	return filepath.Join(paths.StateDir(), "dispatch-relay", paths.Slug(agentID, "", 80, "leaf")+".held")
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

// receiptFix: a job the leaf started is held or attached; relay work without a job passes with a note.
func receiptFix(in *core.Input, _ string, lr *leafRead) (core.Result, bool) {
	st := relayAgent(in)
	if recs, how, ok := leafDispatch(*lr, st); ok {
		return dispatchReceipts(in, lr, recs, how)
	}
	if len(lr.scan.askTimes) > 0 || len(lr.ids) > 0 {
		return missingIDReceipt(in, lr, st)
	}
	model, _ := transcript.LastUsage(in.AgentTranscriptPath)
	return attach(in, fmt.Sprintf("NOTE: ran on the relay model (%s), not %s.", cmp.Or(model.Model, "the leaf's own model"), st), jobOut{}), false
}

func dispatchReceipts(in *core.Input, lr *leafRead, recs []agent.Record, how string) (core.Result, bool) {
	var got []receipt
	var held *core.Result
	for _, rec := range recs {
		r, hold := dispatchReceipt(in.CWD, rec, how, "", *lr)
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

// unattributedNote tops a report whose ask no dispatch record could be tied to.
const unattributedNote = "NOTE: the relay's ask could not be attributed to a dispatch record, so the work may have been done by the relay model."

// Only proven matches can hold the hand-back; other candidates may belong to siblings.
func missingIDReceipt(in *core.Input, lr *leafRead, st string) (core.Result, bool) {
	recs := askRecords(lr.scan.askTimes, st)
	tied := slices.DeleteFunc(slices.Clone(recs), func(r agent.Record) bool { return !askBound(lr.scan.asks, r) })
	if len(tied) > 0 {
		return dispatchReceipts(in, lr, tied, "looked up by session, agent, brief and launch time")
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
	return slices.DeleteFunc(agent.RecordsSince(first), func(r agent.Record) bool {
		return r.Owner == 0 || r.Owner != watchOwner() || r.Agent != agentName || r.Time.Before(first)
	})
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
func dispatchReceipt(cwd string, rec agent.Record, how, output string, lr leafRead) (receipt, *core.Result) {
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
	info, idPrefix := heldInfo(cwd, lr, rec)
	if slices.Contains(lr.scan.heldJobs, rec.ID) {
		return receipt{}, &core.Result{Decision: core.Block, Rule: "job-running", Verdict: core.VerdictHeld, Reason: repeatHold(info, idPrefix, rec.ID)}
	}
	return receipt{}, &core.Result{Decision: core.Block, Rule: "job-running", Verdict: core.VerdictHeld, Reason: fmt.Sprintf("%s %s(%s). %s", info, idPrefix, how, pollOnce(rec.ID))}
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
		m, _, _ := redact.SharedWithin(vaultWait())  // bounded as secrets-out: a stuck keyring must not stall the hand-back
		updated["message"] = m.String(text.String()) // the attached message leaves the process, so mask it
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
	path := paths.MemorySub("reports", time.Now().Format("2006-01-02")+"-handback-"+paths.Slug(agentID, "", 40, "leaf")+".md")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil || paths.AtomicWrite(path, []byte(text), 0o600) != nil {
		return res
	}
	if !strings.Contains(res.Reason, heldAgain) {
		res.Reason += " The report you tried to hand back is saved at " + path + "."
	}
	return res
}

// AskWords is an ask command line parsed by helptext.ParseArgs, the parser `agents ask` itself runs.
type AskWords struct {
	fs  *flag.FlagSet
	Pos []string // the task words
}

// askBoolFlags are ask's boolean flags; every other registered flag takes a value (a test pins both lists to ask's).
var askBoolFlags = strings.Fields("native tools read-only no-tools no-persist direct autonomous fleet " +
	"enable-memory stdin-framed live background fail-fast diff index dry-run loop status fresh recover carry")

// ParseAskWords parses words as ask does (flags anywhere, none after "--"); ok is false where ask would exit 2.
func ParseAskWords(words []string) (AskWords, bool) {
	fs := helptext.NewFlagSet("ask", helptext.Page{})
	fs.SetOutput(io.Discard)
	for _, n := range askBoolFlags {
		fs.Bool(n, false, "")
	}
	for _, n := range askValueFlags {
		fs.String(n, "", "")
	}
	pos, err := helptext.ParseArgs(fs, words)
	return AskWords{fs, pos}, err == nil
}

// Value is the last typed value of flag name, "" when untyped.
func (a AskWords) Value(name string) string {
	v := ""
	a.fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			v = f.Value.String()
		}
	})
	return v
}

// Has reports whether any of names was typed.
func (a AskWords) Has(names ...string) bool {
	found := false
	a.fs.Visit(func(f *flag.Flag) { found = found || slices.Contains(names, f.Name) })
	return found
}

// Prompt is the task words; a lone dash asks for stdin rather than supplying task text.
func (a AskWords) Prompt() string {
	if len(a.Pos) == 0 || len(a.Pos) == 1 && a.Pos[0] == "-" {
		return ""
	}
	return strings.Join(a.Pos, " ")
}

var askValueFlags = strings.Fields("agent agents fallback profile provider as model effort workdir session debug-file " +
	"attach keep-env env permissions overlay system-prompt append-system-prompt output-schema timeout host " +
	"format dirs files max-files max-file-bytes rubric retry job batch jobs save spec " +
	"loop-rounds loop-planner loop-budget-tokens loop-resume waves wave gate profiles codemode")

// OverlayArgs is the ask argv for a profile and a plain model: one --overlay, or --profile when unknown.
func OverlayArgs(profile, model, identity, account string) []string {
	t := overlayTarget(profile, model)
	switch t.Provider {
	case "databricks":
		t.Identity = identity
	case "github":
		t.Account = account
	case "":
		if t.Profile != "" { // no registry row: ask resolves the name and refuses it if unknown
			return append([]string{"--profile", t.Profile}, overlayFlag(route.Target{Model: t.Model})...)
		}
	}
	return overlayFlag(t)
}

// overlayTarget fills the provider from the profile's registry row; a bare provider key is its pool pick.
func overlayTarget(profile, model string) route.Target {
	reg, _ := providers.Load()
	t := route.Target{Route: route.ParseLegacy(profile), Model: model}
	if t.Provider != "" {
		t.Profile = cmp.Or(t.Profile, agent.AutoModel)
	} else if reg != nil && t.Profile != "" {
		if p, err := reg.Get(t.Profile); err == nil {
			t.Provider = p.Provider
		}
	}
	return t
}

func overlayFlag(t route.Target) []string {
	if t.IsZero() && t.Model == "" {
		return nil
	}
	return []string{"--overlay", t.JSON()}
}
