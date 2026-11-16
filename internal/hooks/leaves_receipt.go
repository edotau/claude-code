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
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/transcript"
)

// editTools are the tool calls that change files: a leaf that made one did the work itself.
var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

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
	if rec, how, ok := leafDispatch(lr, st); ok {
		if hookRan && launchedBy(rec.Time, []time.Time{m.Started}) {
			how = "the hook ran the brief's `Run:` line itself"
		}
		return recordReceipt(in, rec, how, m.Output)
	}
	if hookRan {
		return relayRunPending(in, m)
	}
	words := openingAskWords("Run: " + lr.scan.runLine)
	if lr.scan.edited || len(words) == 0 {
		model, _ := transcript.LastUsage(in.AgentTranscriptPath)
		return attach(in, fmt.Sprintf("NOTE: the work was done on the relay model (%s), not on %s; re-run the brief's `Run:` "+
			"line if %s must do it.", cmp.Or(model.Model, "the leaf's own model"), st, st), ""), false
	}
	return runBriefLine(in, agentID, words, lr.scan.runLine)
}

// leafDispatch is the job this leaf started: an id its tool results printed, else a record launched by its ask call.
func leafDispatch(lr leafRead, st string) (rec agent.Record, how string, ok bool) {
	for _, id := range lr.ids {
		if rec, err := loadDispatch(id); err == nil {
			return rec, "printed in its tool results", true
		}
	}
	if len(lr.scan.askTimes) == 0 {
		return agent.Record{}, "", false
	}
	owner := inflight.SessionOwner()
	for _, rec := range recentDispatches(50) {
		mine := owner == 0 || rec.Owner == 0 || rec.Owner == owner
		if mine && (rec.Agent == st || rec.Fallback == st) && launchedBy(rec.Time, lr.scan.askTimes) {
			return rec, "launched by its ask call", true
		}
	}
	return agent.Record{}, "", false
}

// launchedBy is a record start within askLaunchWindow after one of the ask calls (2 s early for clock rounding).
func launchedBy(start time.Time, calls []time.Time) bool {
	return slices.ContainsFunc(calls, func(at time.Time) bool {
		return !start.Before(at.Add(-2*time.Second)) && start.Before(at.Add(askLaunchWindow))
	})
}

// recordReceipt holds while the job runs (up to the cap), then passes with its ran-on line and result attached.
func recordReceipt(in *core.Input, rec agent.Record, how, output string) (core.Result, bool) {
	if rec.Alive() && !pidReused(rec) {
		if time.Since(rec.Time) >= leafStopMaxWait {
			return attach(in, fmt.Sprintf("NOTE: dispatch %s (%s) still running at the %s limit; its result is not in this report "+
				"(`claude-code agents result %s`).", rec.ID, how, leafStopMaxWait, rec.ID), ""), false
		}
		return core.Result{Decision: core.Block, Reason: fmt.Sprintf("Dispatch %s (%s) is still running, so your hand-back is held, "+
			"not refused. Run `claude-code agents result -f %s` with a 600000ms Bash timeout, repeating until it finishes, then hand "+
			"back with its result and `ask: ran on` line.", rec.ID, how, rec.ID)}, true
	}
	body := jobResult(rec.LogPath(), output, rec.ID)
	if line := rec.ResultEnvelope(io.Discard).RanOn; line != "" {
		return attach(in, fmt.Sprintf("NOTE: dispatch %s (%s) supplied the receipt: ask: %s", rec.ID, how, line), body), false
	}
	return attach(in, fmt.Sprintf("NOTE: dispatch %s (%s) ended %s without running an agent.", rec.ID, how, rec.Status), body), false
}

// relayRunPending is the hook's launch with no dispatch record yet: its output decides, else hold while it starts.
func relayRunPending(in *core.Input, m relayRun) (core.Result, bool) {
	out := readEnds(m.Output, resultCap)
	body := jobResult("", m.Output, "")
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
			"this report was made on the relay model.", err), ""), false
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

// jobResult is the job's result text for a hand-back: the hook's output file, else the record's log.
func jobResult(logPath, output, id string) string {
	text := ""
	if output != "" {
		text = readEnds(output, resultCap)
	}
	if strings.TrimSpace(text) == "" && logPath != "" {
		text = readEnds(logPath, resultCap)
	}
	if strings.TrimSpace(text) == "" {
		if id == "" {
			return ""
		}
		text = "(no log; `claude-code agents result " + id + "`)"
	}
	return "\n\n--- job result ---\n" + strings.TrimSpace(text)
}

// attach puts a one-line note on top of the hand-back and the result below it; a stop gets the note only.
func attach(in *core.Input, note, body string) core.Result {
	res := core.Result{SystemMessage: note}
	if in.ToolName != "SubagentHandback" {
		return res
	}
	if updated := in.CloneToolInput(); updated != nil {
		msg, _ := updated["message"].(string)
		updated["message"] = note + "\n\n" + msg + body
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
