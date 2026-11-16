package hooks

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
)

// Regression (2026-11-16): a leaf whose two ask calls printed no ids hands back both jobs, in dispatch order.
func TestLeafDispatchEveryAskCall(t *testing.T) {
	at := time.Now().Add(-10 * time.Minute)
	a := agent.Record{ID: "a", Agent: "codex", Time: at.Add(5 * time.Second)}
	b := agent.Record{ID: "b", Agent: "codex", Time: at.Add(5 * time.Minute)}
	orig := recentDispatches
	t.Cleanup(func() { recentDispatches = orig })
	recentDispatches = func(int) []agent.Record { return []agent.Record{b, a} } // newest first
	var lr leafRead
	lr.scan.askTimes = []time.Time{at, at.Add(5*time.Minute - 5*time.Second)}
	recs, _, ok := leafDispatch(lr, "codex")
	if !ok || len(recs) != 2 || recs[0].ID != "a" || recs[1].ID != "b" {
		t.Fatalf("jobs = %+v ok=%v, want a then b", recs, ok)
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
	orig, poll := recentDispatches, handbackPoll
	t.Cleanup(func() { recentDispatches, handbackPoll = orig, poll })
	recentDispatches, handbackPoll = func(int) []agent.Record { return []agent.Record{second, first} }, time.Millisecond
	gate := func() (core.Result, bool) {
		lr := readLeaf(in.AgentTranscriptPath, leafWants(in))
		lr.scan.askTimes, lr.until = []time.Time{at, at.Add(89 * time.Second)}, time.Now().Add(20*time.Millisecond)
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
