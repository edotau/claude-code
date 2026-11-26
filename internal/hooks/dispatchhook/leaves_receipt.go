// The receipt fix: an external-agent leaf with no `ask: ran on` line is attached or noted; never dropped.
package dispatchhook

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/lane"
	"github.com/edotau/claude-code/internal/hooks/secrets"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// resultCap bounds each end (head and tail) of a job result attached to a hand-back.
const resultCap = 16 << 10

// receiptFix: a job the leaf started is attached or noted; relay work without a job passes with a note.
func receiptFix(in *core.Input, lr *leafRead) core.Result {
	st := relayAgent(in)
	if len(lr.recs) > 0 { // only records named by this leaf's tool results
		return dispatchReceipts(in, lr, lr.recs, "printed in its tool results")
	}
	if len(lr.scan.askTimes) > 0 || len(lr.ids) > 0 {
		return missingIDReceipt(in, lr, st)
	}
	model, _ := transcript.LastUsage(in.AgentTranscriptPath)
	return attach(in, fmt.Sprintf("NOTE: ran on the relay model (%s), not %s.", cmp.Or(model.Model, "the leaf's own model"), st), jobOut{})
}

func dispatchReceipts(in *core.Input, lr *leafRead, recs []agent.Record, how string) core.Result {
	var got []receipt
	for _, rec := range recs {
		got = append(got, dispatchReceipt(rec, how, "", *lr))
	}
	return attachAll(in, got)
}

// unattributedNote tops a report whose ask no dispatch record could be tied to.
const unattributedNote = "NOTE: the relay's ask could not be attributed to a dispatch record, so the work may have been done by the relay model."

// Only proven matches are attached; other candidates may belong to siblings.
func missingIDReceipt(in *core.Input, lr *leafRead, st string) core.Result {
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
	return res
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

// receipt is one dispatch's note line and the result attached under it.
type receipt struct {
	note string
	body jobOut
}

// dispatchReceipt waits for rec; a job still running gets a note, a finished one its receipt.
func dispatchReceipt(rec agent.Record, how, output string, lr leafRead) receipt {
	rec, err := awaitDispatch(rec, lr.until)
	switch {
	case err != nil:
		return receipt{note: fmt.Sprintf("NOTE: dispatch %s (%s) became unreadable while the hand-back waited (%v); its result "+
			"is not attached (`claude-code agents result %s`).", rec.ID, how, err, rec.ID)}
	case !jobRunning(rec):
		return endedNote(rec, how, output)
	}
	return receipt{note: fmt.Sprintf("NOTE: dispatch %s (%s) still running; its result is not in this report "+
		"(`claude-code agents result %s`).", rec.ID, how, rec.ID)}
}

// endedNote is a finished job's ran-on line and result.
func endedNote(rec agent.Record, how, output string) receipt {
	body := jobResult(&rec, output)
	env := rec.ResultEnvelope(io.Discard)
	if line := env.RanOn; line != "" {
		note := fmt.Sprintf("NOTE: dispatch %s (%s) supplied the receipt: ask: %s", rec.ID, how, line)
		if env.Handback != nil && env.Error == nil {
			note += fmt.Sprintf(" handback: %s, unfixed: %s", env.Handback.Status, lane.HandbackVerdict(env.Handback))
		}
		return receipt{note, body}
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
		m, _, _ := redact.SharedWithin(secrets.VaultWait()) // bounded as secrets-out: a stuck keyring must not stall the hand-back
		updated["message"] = m.String(text.String())        // the attached message leaves the process, so mask it
		res.UpdatedInput = updated
	}
	return res
}

// AskWords is an ask command line parsed by helptext.ParseArgs, the parser `agents ask` itself runs.
type AskWords struct {
	fs        *flag.FlagSet
	Pos       []string // the task words
	overlay   map[string]any
	overlayOK bool
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
	a := AskWords{fs: fs, Pos: pos}
	raw := strings.TrimSpace(a.last("overlay"))
	a.overlayOK = strings.HasPrefix(raw, "{") && json.Valid([]byte(raw))
	if a.overlayOK {
		dec := json.NewDecoder(strings.NewReader(raw))
		if _, err := dec.Token(); err != nil {
			return a, false
		}
		a.overlay = map[string]any{}
		for dec.More() {
			key, err := dec.Token()
			var value any
			if err != nil || dec.Decode(&value) != nil {
				return a, false
			}
			for _, name := range overlayRead {
				if strings.EqualFold(key.(string), name) {
					_, isString := value.(string)
					a.overlayOK = a.overlayOK && isString
					a.overlay[name] = value
					break
				}
			}
		}
	}
	return a, err == nil
}

// overlayRead are the flags whose value a hook may also read from an inline --overlay key of the same name.
var overlayRead = []string{"agent", "timeout", "profile", "provider", "model", "tier"}

// Value keeps a typed flag over its overlay key, including an explicitly empty flag.
func (a AskWords) Value(name string) string {
	if a.Has(name) {
		return a.last(name)
	}
	v, _ := a.overlay[name].(string)
	return v
}

// last is the last typed value of flag name, "" when untyped.
func (a AskWords) last(name string) string {
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
	"attach keep-env env overlay system-prompt append-system-prompt output-schema timeout host " +
	"format dirs files max-files max-file-bytes rubric retry job batch jobs save spec brief " +
	"loop-rounds loop-planner loop-budget-tokens loop-resume waves wave gate profiles codemode runner tier plan-task google-tools")

// MaxBashTimeout is the host's Bash tool ceiling; waitMargin lets `agents wait` exit 4 before the tool kills it.
const (
	MaxBashTimeout = 10 * time.Minute
	waitMargin     = 10 * time.Second
	bashDefault    = 2 * time.Minute
)

// BashTimeoutMs is the Bash timeout (ms) a foreground `agents ask|wait|result -f` needs, --timeout + margin, capped.
func BashTimeoutMs(cmd string) (ms int, ok bool) {
	var total time.Duration
	for _, seg := range shell.Segments(cmd) {
		words := agentCommand(relayWords(seg))
		if len(words) < 2 || words[0] != "agents" {
			continue
		}
		t, fg := foregroundTimeout(words[1:])
		if !fg {
			continue
		}
		d := MaxBashTimeout
		if t > 0 {
			d = min(min(t, MaxBashTimeout)+waitMargin, MaxBashTimeout)
		}
		total, ok = min(total+d, MaxBashTimeout), true // chained waits run one after another
	}
	return int(total / time.Millisecond), ok
}

// BashTimeout raises a foreground ask, wait or result -f Bash call's timeout to what it needs; it never blocks.
func BashTimeout(in *core.Input) core.Result {
	ms, ok := BashTimeoutMs(in.ToolCommand())
	updated := in.CloneToolInput()
	if bg, _ := updated["run_in_background"].(bool); in.ToolName != "Bash" || !ok || updated == nil || bg {
		return core.Result{} // a background call has no foreground timeout to raise
	}
	have, set := updated["timeout"].(float64)
	if !set {
		have = float64(bashDefault.Milliseconds()) // an unset timeout is the host default; never shorten it
	}
	if int(have) >= ms {
		return core.Result{}
	}
	if in.PermissionMode == "auto" { // a changed input voids the auto classifier's verdict and forces a re-issue
		return core.Result{Context: fmt.Sprintf("set this call's timeout to %d next time", ms)}
	}
	updated["timeout"] = ms
	return core.Result{UpdatedInput: updated}
}

// foregroundTimeout: words (after `agents`) are a `wait`, `result -f` or ask that stays foreground; t is its --timeout.
func foregroundTimeout(words []string) (t time.Duration, fg bool) {
	switch words[0] {
	case "ask":
		a, ok := ParseAskWords(words[1:])
		bg, _ := strconv.ParseBool(a.Value("background"))
		live, _ := strconv.ParseBool(a.Value("live"))
		if !ok || bg || live {
			return 0, false
		}
		t, _ = time.ParseDuration(a.Value("timeout"))
		return t, true
	case "wait", "result":
		if i := slices.Index(words, "--"); i >= 0 {
			words = words[:i]
		}
		return timeoutFlag(words), words[0] == "wait" || slices.Contains(words, "-f")
	}
	return 0, false
}

// timeoutFlag is the last parseable `--timeout D` or `--timeout=D` in words, 0 when none.
func timeoutFlag(words []string) time.Duration {
	var d time.Duration
	for i, w := range words {
		v, has := strings.CutPrefix(w, "--timeout=")
		if w == "--timeout" && i+1 < len(words) {
			v, has = words[i+1], true
		}
		if t, err := time.ParseDuration(v); has && err == nil {
			d = t
		}
	}
	return d
}
