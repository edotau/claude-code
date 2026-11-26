package dispatchhook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
)

// Exact printed IDs bind multiple jobs; timestamps alone bind none.
func TestLeafDispatchEveryAskCall(t *testing.T) {
	at := time.Now().Add(-10 * time.Minute)
	a := agent.Record{ID: "a", Agent: "codex", Time: at}
	b := agent.Record{ID: "b", Agent: "codex", Time: at.Add(time.Second)}
	in := handbackOf(leafTx(t, "codex", "Objective: x", "output"), "report")
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
		msg, _ := receiptFix(in, &lr).UpdatedInput["message"].(string)
		got := 0
		for _, id := range []string{"a", "b"} {
			if strings.Contains(msg, "dispatch "+id+" (printed in its tool results)") {
				got++
			}
		}
		if got != tc.want {
			t.Errorf("%s: %d printed receipts in %q, want %d", tc.name, got, msg, tc.want)
		}
	}
}

func TestRelayMissingIDCap(t *testing.T) {
	in := handbackOf(leafTx(t, "codex", "Objective: x", "output"), "report")
	old := time.Now().Add(-leafStopMaxWait - time.Minute)
	lr := leafRead{scan: leafScan{askTimes: []time.Time{old}}}
	r := receiptFix(in, &lr)
	if !strings.Contains(r.SystemMessage, unattributedNote) {
		t.Fatalf("HIGH3 cap: %+v", r)
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
	if r := leafStopNote(in, "a1"); r.Decision == core.Block || r.SystemMessage != "" {
		t.Fatalf("ran output: got %+v, want a silent release", r)
	}
	empty := bgOutputFile(t, "working\n")
	if r := leafStopNote(leafTx(t, "codex", brief, bgNotice(empty)), "a1"); r.Decision == core.Block || !strings.Contains(r.SystemMessage, unattributedNote) {
		t.Fatalf("an output with no receipt and no record: got %+v, want the unattributed note", r)
	}
	refused := bgOutputFile(t, "ask: did not run codex: no login\n")
	r := leafStopNote(leafTx(t, "codex", brief, bgNotice(refused)), "a1")
	blocked := r.Decision == core.Block
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
		{"ask exited 1 after running", "gemini", "claude-code agents ask --agent gemini --tools - < /tmp/b.md", failedAsk, gm, true, false},
		{"guard: review envelope names no record", "copilot", "claude-code copilot review --commit 994c08b1 --format json", envelope(cp), "", false, true},
		{"guard: review prints no receipt", "copilot", "claude-code copilot review --commit 994c08b1", "findings only", cp, false, true},
		{"guard: failed ask prints no receipt", "gemini", "claude-code agents ask --agent gemini - < /tmp/b.md", "Exit code 1\nboom", gm, true, true},
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
			r := leafStopNote(leafInput(filepath.Join(dir, "agent-a1.jsonl")), "a1")
			blocked := r.Decision == core.Block
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

// missingIDResult is the gate's verdict on in: the hold reason when blocked (never now), else the hand-back message.
func missingIDResult(in *core.Input) (string, bool) {
	r := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
	blocked := r.Decision == core.Block
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
		{"tied running is noted", map[string]string{a: "running"}, nil, false, []string{"NOTE: dispatch " + a, "still running"}, unattributedNote},
		{"untied running siblings", nil, map[string]string{a: untied(a, at.Add(time.Second)), b: untied(b, at.Add(2*time.Second))}, false,
			[]string{unattributedNote, "Candidates", a, b}, "supplied the receipt"},
		{"two tied, one running, is noted", map[string]string{a: "ok", b: "running"}, nil, false, []string{"NOTE: dispatch " + b, "still running"}, unattributedNote},
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
	r := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
	blocked := r.Decision == core.Block
	msg, _ := r.UpdatedInput["message"].(string)
	if blocked || !strings.HasPrefix(msg, "NOTE: dispatch "+id) || !strings.Contains(msg, "ask: ran on codex") || !strings.Contains(msg, "the relay report") {
		t.Fatalf("got %+v blocked=%v, want the hand-back through with the looked-up receipt and the report", r, blocked)
	}
}

// A tie is the ask's whole task by ask's grammar: flags may follow it, a `--` task counts, a substring or stdin never.
func TestAskTiedEqualsTheWholeTask(t *testing.T) {
	const literal = "audit alpha <memory-recall> marker"
	const recall = "\n\n<memory-recall>\n- x\nLoad one with: claude-code memory search --id <id>\n</memory-recall>"
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
		{"legacy recall footer", `claude-code agents ask --agent codex "audit alpha"`, "audit alpha" + strings.ReplaceAll(recall, "memory search --id", "memory show"), true},
		{"literal marker in brief", `claude-code agents ask --agent codex "` + literal + `"`, literal, true},
		{"literal marker is not a prefix tie", `claude-code agents ask --agent codex "audit alpha"`, literal, false},
		{"flag after the task", "claude-code agents ask --agent codex mine --timeout 1m", "mine", true},
		{"flag value is no task", "claude-code agents ask --agent codex mine --timeout 1m", "wait 1m for it", false},
		{"task after --", `claude-code agents ask --agent codex --timeout 1m -- "-x run"`, "-x run", true},
		{"whitespace normalized", "claude-code agents ask --agent codex 'a   b'", "a b", true},
		{"substring of the prompt", "claude-code agents ask --agent codex audit", "audit alpha", false},
		{"stdin task", "claude-code agents ask --agent codex -", "-", false},
		{"brief file equal", "claude-code agents ask --agent codex --background - < " + brief, "audit alpha", true},
		{"brief file is only a substring", "claude-code agents ask --agent codex - < " + brief, "audit alpha beta", false},
		{"trailing 2>&1 and tail", `claude-code agents ask --agent codex --tools -- "audit alpha" 2>&1 | tail -40`, "audit alpha" + recall, true},
		{"other agent", `claude-code agents ask --agent gemini "audit alpha"`, "audit alpha", false},
	} {
		if got := askTied(c.cmd, rec(c.prompt)); got != c.want {
			t.Errorf("%s: askTied(%q, %q) = %v, want %v", c.name, c.cmd, c.prompt, got, c.want)
		}
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
	r := leafGate(in, "a1", lr)
	blocked := r.Decision == core.Block
	if blocked || !strings.Contains(r.SystemMessage, unattributedNote) {
		t.Fatalf("error ask: %+v blocked=%v", r, blocked)
	}
}

func TestBashTimeoutMs(t *testing.T) {
	for cmd, want := range map[string]int{
		"claude-code agents result -f abc":                       600000,
		"claude-code agents wait abc --timeout 30s":              40000,
		"rtk claude-code agents wait abc --timeout=20m":          600000,
		"claude-code agents ask --agent pi - < BRIEF":            600000,
		"claude-code agents ask --background - < BRIEF":          0,
		`claude-code agents ask --overlay '{"timeout":"30s"}' -`: 40000,
		`claude-code agents ask --overlay='{"timeout":"30s"}' -`: 40000,
		"claude-code agents ask --overlay f.json -":              600000,
		"claude-code agents result abc":                          0,
		"go test ./internal/hooks":                               0,
	} {
		if got, ok := BashTimeoutMs(cmd); got != want || ok != (want > 0) {
			t.Errorf("%q: got %d,%v want %d", cmd, got, ok, want)
		}
	}
}

func TestAskWordsReadOverlay(t *testing.T) {
	for _, c := range []struct{ name, line, agent, provider, profile string }{
		{"flags", "--agent pi --provider databricks --profile qa -", "pi", "databricks", "qa"},
		{"inline", `--overlay {"agent":"pi","provider":"databricks","profile":"qa"} -`, "pi", "databricks", "qa"},
		{"equals", `--overlay={"agent":"pi","profile":"qa"} -`, "pi", "", "qa"},
		{"falls back to flag", `--agent pi --overlay {"profile":"qa"} -`, "pi", "", "qa"},
		{"flags win", `--agent codex --provider google --profile login --overlay {"agent":"pi","provider":"databricks","profile":"qa"} -`, "codex", "google", "login"},
		{"empty flag wins", `--agent= --overlay {"agent":"pi"} -`, "", "", ""},
		{"file is unknown", "--overlay f.json -", "", "", ""},
		{"stdin is unknown", "--overlay - ", "", "", ""},
	} {
		a, ok := ParseAskWords(strings.Fields(c.line))
		if !ok || a.Value("agent") != c.agent || a.Value("provider") != c.provider || a.Value("profile") != c.profile {
			t.Errorf("%s: got %q %q %q ok=%v", c.name, a.Value("agent"), a.Value("provider"), a.Value("profile"), ok)
		}
	}
}

// in auto mode the timeout raise is a note, and a background call is left alone.
func TestBashTimeoutAutoAndBackground(t *testing.T) {
	parse := func(mode string, bg bool) *core.Input {
		return core.ParseInputFor(strings.NewReader(fmt.Sprintf(`{"tool_name":"Bash","permission_mode":%q,`+
			`"tool_input":{"command":"claude-code agents result -f j1","run_in_background":%t}}`, mode, bg)), "")
	}
	if r := BashTimeout(parse("default", false)); r.UpdatedInput["timeout"] == nil {
		t.Fatalf("default mode: %+v, want the timeout raised", r)
	}
	if r := BashTimeout(parse("auto", false)); r.UpdatedInput != nil || !strings.Contains(r.Context, "timeout") {
		t.Fatalf("auto mode: %+v, want only a note", r)
	}
	if r := BashTimeout(parse("default", true)); r.UpdatedInput != nil || r.Context != "" {
		t.Fatalf("background call: %+v, want nothing", r)
	}
}
