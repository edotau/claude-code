package hooks

import (
	"fmt"
	"os"
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
	spawnBriefLine = func(_ []string, _ string, out *os.File) error {
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
