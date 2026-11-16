package hooks

import (
	"os"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
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
