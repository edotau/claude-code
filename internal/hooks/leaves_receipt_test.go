package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/redact"
)

// Exact printed IDs bind multiple jobs; timestamps alone bind none.
func TestLeafDispatchEveryAskCall(t *testing.T) {
	at := time.Now().Add(-10 * time.Minute)
	a := agent.Record{ID: "a", Agent: "codex", Time: at}
	b := agent.Record{ID: "b", Agent: "codex", Time: at.Add(time.Second)}
	for _, tc := range []struct {
		name string
		recs []agent.Record
		want int
	}{
		{"printed ids", []agent.Record{a, b}, 2},
		{"sibling timestamps", nil, 0},
	} {
		lr := leafRead{recs: tc.recs}
		lr.scan.askTimes = []time.Time{at}
		recs, _, ok := leafDispatch(lr, "codex")
		if len(recs) != tc.want || ok != (tc.want > 0) {
			t.Errorf("%s: jobs = %+v ok=%v", tc.name, recs, ok)
		}
	}
}

// Regression (2026-11-16): receiptFix's hold on a running job saves the finished one's answer; the pass attaches both.
func TestReceiptFixHoldKeepsFinishedAnswer(t *testing.T) {
	in := handbackOf(leafTx(t, "codex", "Objective: x\nAgent: codex\n", "ls output"), "relay report")
	in.CWD = t.TempDir()
	_, hold := stubTwoJobs(t, in)
	*hold = true
	at := time.Now().Add(-3 * time.Minute)
	first, _ := loadDispatch("first")
	second, _ := loadDispatch("second")
	first.Time, second.Time = at.Add(time.Second), at.Add(90*time.Second)
	poll := handbackPoll
	t.Cleanup(func() { handbackPoll = poll })
	handbackPoll = time.Millisecond
	gate := func() (core.Result, bool) {
		lr := readLeaf(in.AgentTranscriptPath, leafWants(in))
		lr.ids, lr.until = []string{first.ID, second.ID}, time.Now().Add(20*time.Millisecond)
		return leafGate(in, "a1", lr)
	}
	r, blocked := gate()
	_, path, _ := strings.Cut(r.Reason, "is saved at ")
	saved, _ := os.ReadFile(strings.TrimSuffix(path, "."))
	if !blocked || !strings.Contains(r.Reason, "Job second") || !strings.Contains(string(saved), "FINAL ANSWER first") || !wasHeld("a1") {
		t.Fatalf("hold = %+v blocked=%v, saved %q: want the finished answer saved with the report and the marker", r, blocked, saved)
	}
	*hold = false
	r, blocked = gate()
	msg, _ := r.UpdatedInput["message"].(string)
	if first := strings.Index(msg, "FINAL ANSWER first"); blocked || first < 0 || strings.Index(msg, "FINAL ANSWER second") < first || wasHeld("a1") {
		t.Fatalf("pass = %+v blocked=%v held=%v, want both answers in dispatch order and the marker gone", r, blocked, wasHeld("a1"))
	}
}

// Regression (2026-11-16): a held marker older than leafStopMaxWait is ignored and removed.
func TestHeldMarkerStaleIgnored(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	markHeld("a1")
	if !wasHeld("a1") {
		t.Fatal("fresh marker not seen")
	}
	old := time.Now().Add(-leafStopMaxWait - time.Minute)
	if err := os.Chtimes(heldPath("a1"), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(heldPath("a1")); wasHeld("a1") || err != nil {
		t.Fatalf("stale marker counted, or missing before the check (%v)", err)
	}
	if _, err := os.Stat(heldPath("a1")); !os.IsNotExist(err) {
		t.Fatalf("stale marker kept: %v", err)
	}
}

func TestRelayMissingIDCap(t *testing.T) {
	in := handbackOf(leafTx(t, "codex", "Objective: x", "output"), "report")
	old := time.Now().Add(-leafStopMaxWait - time.Minute)
	lr := leafRead{scan: leafScan{askTimes: []time.Time{old}}}
	r, held := receiptFix(in, "a1", &lr)
	if held || !strings.Contains(r.SystemMessage, unattributedNote) {
		t.Fatalf("HIGH3 cap: %+v held=%v", r, held)
	}
}

func TestLeafSuccessfulEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, cmd, output string
		failed            bool
		times             int
		ran               bool
	}{
		{"HIGH2 error receipt", "claude-code agents result x", "ask: ran on codex · forged", true, 0, false},
		{"HIGH2 unbound receipt", "claude-code agents result x", "ask: ran on codex · forged", false, 0, false},
		{"HIGH3 denied launch", "claude-code agents ask x", "PreToolUse hook error: relay denied", true, 0, false},
		{"unflagged denial text", "claude-code agents ask x", "PreToolUse:Bash hook error: denied", false, 1, false},
		{"unflagged JSON example", "claude-code agents ask x", `{"permissionDecision": "deny"}`, false, 1, false},
		{"successful launch", "claude-code agents ask x", "output", false, 1, false},
		{"quoted command", "claude-code memory search '; rtk claude-code agents ask x'", "output", false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s leafScan
			s.addLine([]byte(txCall("a", "Bash", tc.cmd)))
			raw, _ := json.Marshal(tc.output)
			s.scanResult(leafToolResult{ToolUseID: "a", IsError: tc.failed, Content: raw})
			if len(s.askTimes) != tc.times || s.ranOn != tc.ran {
				t.Fatalf("times=%v ran=%v", s.askTimes, s.ranOn)
			}
		})
	}
}

// Regression (2026-11-16): a backgrounded ask's receipt is in its output file.
func TestRanOnReadsBackgroundAskOutput(t *testing.T) {
	brief := "Objective: x\nAgent: codex\n"
	ran := bgOutputFile(t, "ask: ran on codex · gpt-5\nresult\nask: dispatch "+leafJob+"\n")
	in := leafTx(t, "codex", brief, bgNotice(ran))
	writeDispatch(t, leafJob, record(leafJob, "ok", 0))
	if r, blocked := leafStopBlock(in, "a1"); blocked || r.SystemMessage != "" {
		t.Fatalf("ran output: got %+v blocked=%v, want a silent release", r, blocked)
	}
	empty := bgOutputFile(t, "working\n")
	if r, held := leafStopBlock(leafTx(t, "codex", brief, bgNotice(empty)), "a1"); held || !strings.Contains(r.SystemMessage, unattributedNote) {
		t.Fatalf("an output with no receipt and no record: got %+v, want the unattributed note", r)
	}
	refused := bgOutputFile(t, "ask: did not run codex: no login\n")
	r, blocked := leafStopBlock(leafTx(t, "codex", brief, bgNotice(refused)), "a1")
	if blocked || !strings.Contains(r.SystemMessage, "did not run an agent") {
		t.Fatalf("did-not-run output: got %+v blocked=%v, want the did-not-run note", r, blocked)
	}
}

// Regression (2026-11-17): a `<agent> review` call, or an ask that exited non-zero, still shows the vendor ran.
func TestRanOnReceiptFromReviewAndFailedAsk(t *testing.T) {
	const cp, gm = "20261117T193443.389-copilot", "20261117T193444.216-gemini"
	envelope := func(id string) string {
		return `copilot: GitHub account x (gh auth token -u x)` + "\n" +
			`{"schema":1,"status":"ok","dispatch_id":"` + id + `","ran_on":"ran on copilot · github·x · gpt · fallback none"}`
	}
	failedAsk := "Exit code 1\n**Verdict:** ship\n\nask: ran on gemini · google·x · gemini · fallback none\nask: dispatch " + gm +
		"\nclaude-code: ask: gemini: failed: signal: killed\nAPI error (attempt 1): request failed"
	for _, tc := range []struct {
		name, agent, command, result, record string
		isErr, wantNote                      bool
	}{
		{"review printed its envelope", "copilot", "claude-code copilot review --commit 994c08b1 --model gpt --format json", envelope(cp), cp, false, false},
		{"ask exited 1 after running", "gemini", "claude-code gemini ask --tools - < /tmp/b.md", failedAsk, gm, true, false},
		{"guard: review envelope names no record", "copilot", "claude-code copilot review --commit 994c08b1 --format json", envelope(cp), "", false, true},
		{"guard: review prints no receipt", "copilot", "claude-code copilot review --commit 994c08b1", "findings only", cp, false, true},
		{"guard: failed ask prints no receipt", "gemini", "claude-code gemini ask - < /tmp/b.md", "Exit code 1\nboom", gm, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			dir := t.TempDir()
			opening := "review the commit"
			if strings.Contains(tc.command, " review ") { // a Run: line only on the copilot rows: the gemini brief had none
				opening = "Run: " + tc.command + "\n\n" + opening
			}
			brief, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": opening}})
			res, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "t1", "is_error": tc.isErr, "content": tc.result}}}})
			body := string(brief) + "\n" + txCall("t1", "Bash", tc.command) + "\n" + string(res) + "\n"
			if err := os.WriteFile(filepath.Join(dir, "agent-a1.jsonl"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "agent-a1.meta.json"), []byte(`{"agentType":"`+tc.agent+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.record != "" {
				writeDispatch(t, tc.record, record(tc.record, "ok", 0))
			}
			r, blocked := leafStopBlock(leafInput(filepath.Join(dir, "agent-a1.jsonl")), "a1")
			note := relayNote
			if tc.isErr {
				note = unattributedNote
			}
			if got := strings.Contains(r.SystemMessage, note); got != tc.wantNote || blocked {
				t.Errorf("got %+v blocked=%v, want the relay note %v and no block", r, blocked, tc.wantNote)
			}
		})
	}
}

// missingIDTx is a hand-back after an ask whose result printed no id, with this session's records written by rows.
func missingIDTx(t *testing.T, at time.Time, rows map[string]string) *core.Input {
	t.Helper()
	oldOwner := watchOwner
	t.Cleanup(func() { watchOwner = oldOwner })
	watchOwner = func() int { return 4242 }
	in := handbackOf(timedLeafTx(t, at, "answer text"), "relay report")
	in.CWD = t.TempDir()
	for id, status := range rows {
		start := at.Add(time.Second)
		if status == "running" {
			start = at.Add(20 * time.Second)
		}
		writeDispatch(t, id, fmt.Sprintf(`{"id":%q,"time":%q,"agent":"codex","status":%q,"pid":%d,"owner":4242,"model":"m","request":{"prompt":"x"}}`,
			id, start.Format(time.RFC3339), status, os.Getpid()))
	}
	return in
}

// missingIDResult is the gate's verdict on in: the hold reason when blocked, else the attached hand-back message.
func missingIDResult(in *core.Input) (string, bool) {
	r, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
	if blocked {
		return r.Reason, true
	}
	msg, _ := r.UpdatedInput["message"].(string)
	return msg, false
}

func TestReceiptFixMissingIDBindsOnlyOnProof(t *testing.T) {
	const a, b = "20261116T120000.000-codex", "20261116T120010.000-codex"
	at := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	recAs := func(status, id, prompt string, start time.Time) string {
		return fmt.Sprintf(`{"id":%q,"time":%q,"agent":"codex","status":%q,"pid":%d,"owner":4242,"model":"m","request":{"prompt":%q}}`,
			id, start.Format(time.RFC3339), status, os.Getpid(), prompt)
	}
	untied := func(id string, start time.Time) string { return recAs("running", id, "sibling", start) }
	for _, tc := range []struct {
		name      string
		rows      map[string]string
		override  map[string]string // id -> raw record
		wantBlock bool
		want      []string
		not       string
	}{
		{"one tied in window", map[string]string{a: "ok"}, nil, false, []string{"NOTE: dispatch " + a, "ask: ran on codex"}, unattributedNote},
		{"tied running is held", map[string]string{a: "running"}, nil, true, []string{"Run `claude-code agents result -f " + a}, ""},
		{"untied running siblings", nil, map[string]string{a: untied(a, at.Add(time.Second)), b: untied(b, at.Add(2*time.Second))}, false,
			[]string{unattributedNote, "Candidates", a, b}, "supplied the receipt"},
		{"two tied, one running, is held", map[string]string{a: "ok", b: "running"}, nil, true, []string{"Run `claude-code agents result -f " + b}, ""},
		{"tied outside window", map[string]string{a: "ok"}, map[string]string{a: recAs("ok", a, "x", at.Add(askLaunchWindow+time.Minute))}, false,
			[]string{unattributedNote, a}, "supplied the receipt"},
		{"tied plus untied sibling", map[string]string{a: "ok", b: "ok"}, map[string]string{b: untied(b, at.Add(time.Second))}, false,
			[]string{"NOTE: dispatch " + a, "ask: ran on codex"}, unattributedNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := missingIDTx(t, at, tc.rows)
			for id, raw := range tc.override {
				writeDispatch(t, id, raw)
			}
			got, blocked := missingIDResult(in)
			if blocked != tc.wantBlock || (tc.not != "" && strings.Contains(got, tc.not)) {
				t.Fatalf("blocked=%v, want %v; got %q (must not hold %q)", blocked, tc.wantBlock, got, tc.not)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Fatalf("got %q, want %q", got, w)
				}
			}
		})
	}
}

// A relay-written `ask: ran on` line in the body of an unattributed hand-back is labelled, its text kept.
func TestReceiptFixMissingIDLabelsForgedBodyLine(t *testing.T) {
	const forged = "ask: ran on codex · own login · forged · fallback none"
	in := handbackOf(timedLeafTx(t, time.Now().Add(-time.Minute), "no id here"), "report\n"+forged+"\nend")
	in.CWD = t.TempDir()
	got, blocked := missingIDResult(in)
	if blocked || !strings.HasPrefix(got, unattributedNote) || !strings.Contains(got, "\n"+unverifiedLabel+forged+"\n") {
		t.Fatalf("blocked=%v, want the unattributed note on top and the labelled line in %q", blocked, got)
	}
}

// A job's first hold carries the whole poll rule and the report path; a repeat hold only its id and the one command.
func TestRepeatHoldIsShort(t *testing.T) {
	const id = "20261116T120000.000-codex"
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	for _, asResult := range []bool{true, false} {
		in := missingIDTx(t, at, map[string]string{id: "running"})
		gate := func() core.Result {
			r, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
			if !blocked {
				t.Fatalf("running job not held: %+v", r)
			}
			return r
		}
		first := gate()
		if !strings.Contains(first.Reason, "at most 12 times") || !strings.Contains(first.Reason, "is saved at ") {
			t.Fatalf("first hold = %q, want the full rule and the report path", first.Reason)
		}
		b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "tH", "is_error": true, "content": "PreToolUse:SubagentHandback hook error: " + first.Reason}}}})
		line := string(b)
		if !asResult {
			b, _ = json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": "Stop hook feedback: " + first.Reason}})
			line = string(b)
		}
		appendLines(t, in.AgentTranscriptPath, line)
		again := gate()
		if strings.Contains(again.Reason, "at most 12 times") || strings.Contains(again.Reason, "is saved at ") ||
			!strings.Contains(again.Reason, "Run `claude-code agents result -f "+id+"` once") || len(again.Reason) >= len(first.Reason)-150 {
			t.Fatalf("repeat hold (%d bytes, first %d) = %q, want the short form with the command", len(again.Reason), len(first.Reason), again.Reason)
		}
		t.Logf("first %d bytes, repeat %d bytes\n%s\n%s", len(first.Reason), len(again.Reason), first.Reason, again.Reason)
	}
}

// A tool result that only quotes a hold's poll instruction is no earlier hold: the first hold stays the full form.
func TestQuotedPollInstructionIsNoHold(t *testing.T) {
	const id = "20261116T120000.000-codex"
	in := missingIDTx(t, time.Now().Add(-time.Minute).Truncate(time.Second), map[string]string{id: "running"})
	appendLines(t, in.AgentTranscriptPath, txResult("tQ", "docs: "+pollOnce(id)))
	got, blocked := missingIDResult(in)
	if !blocked || !strings.Contains(got, "at most 12 times") || !strings.Contains(got, "is saved at ") {
		t.Fatalf("blocked=%v reason=%q, want the full first hold", blocked, got)
	}
}

// Today's case: an ask result with no id line and a finished record: the hand-back carries the looked-up receipt.
func TestHandbackMissingIDLooksUpRecord(t *testing.T) {
	const id = "20261116T120000.000-codex"
	oldOwner := watchOwner
	t.Cleanup(func() { watchOwner = oldOwner })
	watchOwner = func() int { return 4242 }
	at := time.Now().Add(-21 * time.Minute).Truncate(time.Second)
	in := handbackOf(timedLeafTx(t, at, bgNotice("/nonexistent/tasks/bX1.output")), "the relay report")
	in.CWD = t.TempDir()
	writeDispatch(t, id, fmt.Sprintf(`{"id":%q,"time":%q,"agent":"codex","status":"ok","pid":%d,"owner":4242,"model":"m","provider":"prod","request":{"prompt":"x"}}`,
		id, at.Add(time.Second).Format(time.RFC3339), os.Getpid()))
	r, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
	msg, _ := r.UpdatedInput["message"].(string)
	if blocked || !strings.HasPrefix(msg, "NOTE: dispatch "+id) || !strings.Contains(msg, "ask: ran on codex") || !strings.Contains(msg, "the relay report") {
		t.Fatalf("got %+v blocked=%v, want the hand-back through with the looked-up receipt and the report", r, blocked)
	}
}

// A tie is the ask's whole task by ask's grammar: flags may follow it, a `--` task counts, a substring or stdin never.
func TestAskTiedEqualsTheWholeTask(t *testing.T) {
	const literal = "audit alpha <memory-recall> marker"
	const recall = "\n\n<memory-recall>\n- x\nLoad one with: claude-code memory show <id>\n</memory-recall>"
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("audit alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := func(prompt string) agent.Record {
		var r agent.Record
		r.Agent, r.Request.Prompt = "codex", prompt
		return r
	}
	for _, c := range []struct {
		name, cmd, prompt string
		want              bool
	}{
		{"equal task", `claude-code agents ask --agent codex "audit alpha"`, "audit alpha", true},
		{"recall block appended", `claude-code agents ask --agent codex "audit alpha"`, "audit alpha" + recall, true},
		{"literal marker in brief", `claude-code agents ask --agent codex "` + literal + `"`, literal, true},
		{"literal marker is not a prefix tie", `claude-code agents ask --agent codex "audit alpha"`, literal, false},
		{"flag after the task", "claude-code agents ask --agent codex mine --timeout 1m", "mine", true},
		{"flag value is no task", "claude-code agents ask --agent codex mine --timeout 1m", "wait 1m for it", false},
		{"task after --", `claude-code agents ask --agent codex --timeout 1m -- "-x run"`, "-x run", true},
		{"whitespace normalized", "claude-code codex ask 'a   b'", "a b", true},
		{"substring of the prompt", "claude-code agents ask --agent codex audit", "audit alpha", false},
		{"stdin task", "claude-code agents ask --agent codex -", "-", false},
		{"brief file equal", "claude-code agents ask --agent codex --background - < " + brief, "audit alpha", true},
		{"brief file is only a substring", "claude-code agents ask --agent codex - < " + brief, "audit alpha beta", false},
		{"trailing 2>&1 and tail", `claude-code codex ask --tools -- "audit alpha" 2>&1 | tail -40`, "audit alpha" + recall, true},
		{"other agent", `claude-code agents ask --agent gemini "audit alpha"`, "audit alpha", false},
	} {
		if got := askTied(c.cmd, rec(c.prompt)); got != c.want {
			t.Errorf("%s: askTied(%q, %q) = %v, want %v", c.name, c.cmd, c.prompt, got, c.want)
		}
	}
}

func TestLeafDeniedAskHolds(t *testing.T) {
	for _, prefix := range []string{"PreToolUse hook error: ", "PreToolUse:Bash hook error: "} {
		var s leafScan
		s.addLine([]byte(txCall("a", "Bash", "claude-code agents ask x")))
		raw, err := json.Marshal(prefix + pollOnce("20261118T120000.000-codex"))
		if err != nil {
			t.Fatal(err)
		}
		s.addLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a","is_error":true,"content":` + string(raw) + `}]}}`))
		if len(s.askTimes) != 0 || len(s.heldJobs) != 1 {
			t.Fatalf("denied ask: %+v", s)
		}
	}
	var s leafScan
	s.addLine([]byte(txCall("a", "Bash", "claude-code agents ask x")))
	s.addLine([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"a","is_error":true,"content":"exit status 1"}]}}`))
	if len(s.askTimes) != 1 || len(s.heldJobs) != 0 {
		t.Fatalf("ordinary error: %+v", s)
	}
}

func TestLeafErroredAskNoRelaunch(t *testing.T) {
	in := timedLeafTx(t, time.Now().Add(-time.Minute), "unused")
	raw, err := os.ReadFile(in.AgentTranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `"content":"unused"`, `"is_error":true,"content":"exit status 1"`, 1))
	if err := os.WriteFile(in.AgentTranscriptPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	lr := readLeaf(in.AgentTranscriptPath, leafWants(in), in.CWD)
	r, blocked := leafGate(in, "a1", lr)
	if blocked || !strings.Contains(r.SystemMessage, unattributedNote) {
		t.Fatalf("error ask: %+v blocked=%v", r, blocked)
	}
}

// A saved hand-back report never holds a vault value; the mask keeps the byte length.
func TestPreserveReportMasksVaultValue(t *testing.T) {
	value := "FAKE-SECRET-0123456789abcdef"
	redact.StubValues(t, []string{value})
	in := handbackOf(leafTx(t, "codex", "Objective: x\nAgent: codex\n", "ls output"), "report with "+value)
	res := preserveReport(in, "a-mask", leafRead{}, core.Result{})
	_, path, _ := strings.Cut(res.Reason, "is saved at ")
	saved, err := os.ReadFile(strings.TrimSuffix(path, "."))
	if err != nil {
		t.Fatalf("report not saved: %v (reason %q)", err, res.Reason)
	}
	if strings.Contains(string(saved), value) || !strings.Contains(string(saved), "[redacted]"+strings.Repeat("*", len(value)-10)) {
		t.Fatalf("saved report = %q, want the value masked at its own length", saved)
	}
}

func TestHeldHoldMessageTaskAndJobForms(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	repo, _ := planRepo(t)
	rec := agent.Record{ID: "j1", Agent: "codex", Time: time.Now(), PlanTask: "1.2"}
	lead, _ := heldInfo(repo, leafRead{}, rec)
	if !strings.HasPrefix(lead, "Info: Task 1.2 running (codex, ") || !strings.HasSuffix(lead, "): claude-code agents ask --agent codex; hand-back waits.") {
		t.Errorf("task form: %q", lead)
	}
	rec.PlanTask = ""
	lead, prefix := heldInfo(t.TempDir(), leafRead{}, rec)
	res, _ := leafStopVerdict("a1", rec, nil, time.Time{}, lead, prefix)
	if !strings.HasPrefix(lead, "Info: Job j1 running (codex, ") || res.Decision != core.Block || res.Verdict != core.VerdictHeld {
		t.Fatalf("job form: lead %q, result %+v, want a held Job lead", lead, res)
	}
	for _, bad := range []string{"Error", "refused", "the brief's Run:"} {
		if strings.Contains(res.Reason, bad) {
			t.Errorf("hold %q contains %q", res.Reason, bad)
		}
	}
}

func TestBashTimeoutMs(t *testing.T) {
	for cmd, want := range map[string]int{
		"claude-code agents result -f abc":              600000,
		"claude-code agents wait abc --timeout 30s":     40000,
		"rtk claude-code agents wait abc --timeout=20m": 600000,
		"claude-code agents ask --agent pi - < BRIEF":   600000,
		"claude-code agents ask --background - < BRIEF": 0,
		"claude-code agents result abc":                 0,
		"go test ./internal/hooks":                      0,
	} {
		if got, ok := BashTimeoutMs(cmd); got != want || ok != (want > 0) {
			t.Errorf("%q: got %d,%v want %d", cmd, got, ok, want)
		}
	}
}

func TestBashTimeoutComposesWithRewrite(t *testing.T) {
	m := &dispatchMerge{timeoutInput: map[string]any{"command": "old", "timeout": 40000}}
	m.merged.UpdatedInput = map[string]any{"command": "rtk new"}
	m.applyTimeout()
	if got := m.merged.UpdatedInput; got["command"] != "rtk new" || got["timeout"] != 40000 {
		t.Errorf("composed input = %v", got)
	}
	for cmd, want := range map[string]int{
		"claude-code agents ask --agent pi -- --timeout=1s": 600000,
		"claude-code agents ask --timeout 30s -":            40000,
		"claude-code agents ask --background=false -":       600000,
		"claude-code agents wait j --timeout 2562047h":      600000,
	} {
		if got, _ := BashTimeoutMs(cmd); got != want {
			t.Errorf("%q: got %d want %d", cmd, got, want)
		}
	}
}

func TestHandbackVerdictReadsTypedStatus(t *testing.T) {
	for h, want := range map[string]wire.Handback{
		"none":    {Status: "DONE", Summary: "s"},
		"BLOCKED": {Status: "BLOCKED", Summary: "s"},
		"x; y":    {Status: "DONE", Unfixed: []string{"x", "y"}},
	} {
		if got := handbackVerdict(&want); got != h {
			t.Errorf("handbackVerdict(%+v) = %q, want %q", want, got, h)
		}
	}
}
