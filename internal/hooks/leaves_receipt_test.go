package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
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
	if !blocked || !strings.Contains(r.Reason, "Dispatch second") || !strings.Contains(string(saved), "FINAL ANSWER first") || !wasHeld("a1") {
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

// The hook's own Run: launch holds naming its job's `agents result -f <id>` once the record appears, never a sleep.
func TestRunBriefLineHoldNamesJob(t *testing.T) {
	stubSpawn(t)
	in := handbackOf(leafTx(t, "codex", "Run: claude-code agents ask --agent codex 'x'\n", "ls output"), "report")
	const id = "20261116T130000.000-codex"
	spawnBriefLine = func(_ []string, _, _ string, out *os.File) error {
		if err := agent.ClaimDispatchID(out.Name()+".claim", id); err != nil {
			return err
		}
		writeDispatch(t, id, record(id, "running", os.Getpid()))
		return nil
	}
	var lr leafRead
	lr.scan.runLine = "claude-code agents ask --agent codex 'x'"
	r, held := runBriefLine(in, lr, "a1", []string{"claude-code", "agents", "ask", "--agent", "codex", "x"})
	if !held || !strings.Contains(r.Reason, "`claude-code agents result -f "+id+"` once") || strings.Contains(r.Reason, "sleep") {
		t.Fatalf("hold = %+v held=%v, want one naming `agents result -f %s` once and no sleep", r, held, id)
	}
}

// A never-run Run: line is run by the hook with the brief as its task; the hand-back is held, then passes with result.
func TestRanOnHookRunsTheLine(t *testing.T) {
	for _, c := range []struct {
		st, line, body string
		want           []string
	}{
		{"codex", "claude-code agents ask --agent codex --tools - < /tmp/x.md", "Objective: review\n",
			[]string{"claude-code", "agents", "ask", "--agent", "codex", "--tools", "-"}},
		{"codex", "claude-code agents ask --agent codex --model gpt-6-1-sol 'review it'", "Objective: review\n",
			[]string{"claude-code", "agents", "ask", "--agent", "codex", "--model", "gpt-6-1-sol", "review it"}},
		{"codex", "claude-code agents ask --agent codex --model x", "Objective: review\n",
			[]string{"claude-code", "agents", "ask", "--agent", "codex", "--model", "x", "Objective: review"}},
		{"codex", "claude-code agents ask --agent codex --profile p", "Objective: review\n",
			[]string{"claude-code", "agents", "ask", "--agent", "codex", "--profile", "p", "Objective: review"}},
		{"codex", "claude-code agents ask --agent codex --tools", "Objective: review\n",
			[]string{"claude-code", "agents", "ask", "--agent", "codex", "--tools", "Objective: review"}},
		{"claude", "claude-code agents ask --agent claude --profile dev --model m --tools", "Objective: review\n",
			[]string{"claude-code", "agents", "ask", "--agent", "claude", "--profile", "dev", "--model", "m", "--tools", "Objective: review"}},
	} {
		spawned := stubSpawn(t)
		in := leafTx(t, c.st, "Run: "+c.line+"\n\n"+c.body, "ls output")
		in = handbackOf(in, "my report")
		r, blocked := leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
		if !blocked || !strings.Contains(r.Reason, "held, not refused") || len(*spawned) != 1 || fmt.Sprint((*spawned)[0]) != fmt.Sprint(c.want) {
			t.Fatalf("%s first hand-back = %+v blocked=%v spawns %v, want one held launch of %v", c.st, r, blocked, *spawned, c.want)
		}
		m, _ := loadRelayRun("a1")
		if err := os.WriteFile(m.Output, []byte("the answer\nask: ran on codex · databricks·prod · databricks-gpt-6-1-sol · fallback none\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r, blocked = leafGate(in, "a1", readLeaf(in.AgentTranscriptPath, leafWants(in)))
		msg, _ := r.UpdatedInput["message"].(string)
		if blocked || len(*spawned) != 1 || !strings.HasPrefix(msg, "NOTE: the hook ran the brief's `Run:` line itself: ask: ran on codex") ||
			!strings.Contains(msg, "my report") || !strings.Contains(msg, "the answer") {
			t.Fatalf("%s second hand-back = %+v blocked=%v, want it through with note, report and result, no second launch", c.st, r, blocked)
		}
	}
}

func TestRelayMissingIDCap(t *testing.T) {
	for _, hook := range []bool{false, true} {
		in := handbackOf(leafTx(t, "codex", "Objective: x", "output"), "report")
		old := time.Now().Add(-leafStopMaxWait - time.Minute)
		lr := leafRead{scan: leafScan{askTimes: []time.Time{old}}}
		if hook {
			if err := os.MkdirAll(filepath.Dir(relayRunPath("a1")), 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := claimRelayRun("a1", relayRun{Started: old, Output: "/nonexistent"}); err != nil {
				t.Fatal(err)
			}
		}
		r, held := receiptFix(in, "a1", &lr)
		if held || !strings.Contains(r.SystemMessage, "limit") {
			t.Fatalf("HIGH3 cap hook=%v: %+v held=%v", hook, r, held)
		}
	}
}

func TestSpawnBriefStdin(t *testing.T) {
	for _, input := range []string{"- < input.md"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "input.md"), []byte("brief body"), 0600); err != nil {
			t.Fatal(err)
		}
		out, err := os.CreateTemp(dir, "output")
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		if err := spawnDetached([]string{"/bin/cat", "-"}, dir, "input.md", out); err != nil {
			t.Fatal(err)
		}
		until := time.Now().Add(time.Second)
		for time.Now().Before(until) && readEnds(out.Name(), 100) != "brief body" {
			time.Sleep(time.Millisecond)
		}
		if got := readEnds(out.Name(), 100); got != "brief body" {
			t.Fatalf("HIGH5 %s: %q", input, got)
		}
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
		{"hook deny without error flag", "claude-code agents ask x", "PreToolUse:Bash hook error: denied", false, 0, false},
		{"structured deny", "claude-code agents ask x", `{"permissionDecision": "deny"}`, false, 0, false},
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

func TestScratchContainment(t *testing.T) {
	hosttest.SkipOnWindows(t, "sh scripts, symlinks or errno")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "outside")
	if err := os.Symlink(cwd, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/private/tmp/../../Users/edotau/.claude/internal/hooks/leaves.go", false},
		{filepath.Join(link, "leaves.go"), false},
		{filepath.Join(link, "nonexistent.go"), false},
		{filepath.Join(dir, "new.md"), true},
	} {
		if got := scratchPath(tc.path); got != tc.want {
			t.Errorf("%s scratch=%v", tc.path, got)
		}
	}
}

func TestEditEvidence(t *testing.T) {
	for _, tc := range []struct {
		path         string
		failed, want bool
	}{
		{"internal/hooks/leaves.go", true, false},
		{"internal/hooks/leaves.go", false, true},
		{"memory/reports/x.md", false, false},
	} {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		path := tc.path
		if strings.HasPrefix(path, "memory/") {
			path, err = filepath.Rel(cwd, memory.Sub("reports", "x.md"))
			if err != nil {
				t.Fatal(err)
			}
		}
		s := leafScan{cwd: cwd}
		content, _ := json.Marshal([]map[string]any{{"type": "tool_use", "id": "edit", "name": "Edit", "input": map[string]string{"file_path": path}}})
		s.addCalls(content, time.Now())
		if s.edited {
			t.Fatal("edit counted before result")
		}
		s.scanResult(leafToolResult{ToolUseID: "edit", IsError: tc.failed, Content: json.RawMessage(`"edited"`)})
		if s.edited != tc.want {
			t.Errorf("%s failed=%v edited=%v", tc.path, tc.failed, s.edited)
		}
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
	if r, held := leafStopBlock(leafTx(t, "codex", brief, bgNotice(empty)), "a1"); !held || !strings.Contains(r.Reason, "exact dispatch id") {
		t.Fatalf("an output with no receipt and no record: got %+v, want an exact-id hold", r)
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
		{"ask exited 1 after running", "gemini", "claude-code gemini ask --tools --read-only - < /tmp/b.md", failedAsk, gm, true, false},
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
			if got := strings.Contains(r.SystemMessage, relayNote); got != tc.wantNote || blocked {
				t.Errorf("got %+v blocked=%v, want the relay note %v and no block", r, blocked, tc.wantNote)
			}
		})
	}
}
