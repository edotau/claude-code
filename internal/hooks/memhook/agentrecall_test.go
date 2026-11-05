package memhook

import (
	"fmt"
	"strings"
	"testing"
)

// TestCapRecallLinesDropsWholeTrailingLinesToFitByteBudget: capRecallLines is the last line of
// defense once a future preview widening pushes a block over budget — it must drop whole trailing
// lines (never truncate one mid-string) until the block fits, and return the matching kept count.
func TestCapRecallLinesDropsWholeTrailingLinesToFitByteBudget(t *testing.T) {
	long := "\n- " + strings.Repeat("x", 350)
	block, n := capRecallLines([]string{long, long, long})
	if n != 2 || strings.Count(block, long) != 2 {
		t.Fatalf("kept n=%d, want 2 whole lines: %q", n, block)
	}
	if len(block) > agentRecallBlockCapBytes {
		t.Errorf("block is %d bytes, want <= %d", len(block), agentRecallBlockCapBytes)
	}
	if strings.Contains(block, "x…") || strings.Contains(block, "…") {
		t.Errorf("a line was cut mid-string instead of dropped whole: %q", block)
	}
	if block, n := capRecallLines(nil); block != "" || n != 0 {
		t.Errorf("capRecallLines(nil) = (%q, %d), want (\"\", 0)", block, n)
	}
	if _, n := capRecallLines([]string{strings.Repeat("y", 2000)}); n != 0 {
		t.Errorf("a single line too big alone must drop to empty, got n=%d", n)
	}
}

const recallTask = "stage the deploy pipeline staging signoff rollout"

// seedRecallBank writes n id-marked, strongly matching home-bank blocks so AgentRecall has real hits to rank.
func seedRecallBank(t *testing.T, n int) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# Conventions\n\n")
	for i := range n {
		fmt.Fprintf(&b, "- deploy pipeline staging signoff rule %d %s<!-- id:%08x -->\n", i, strings.Repeat("padding ", 40), i+1)
	}
	writeBank(t, "conventions.md", b.String())
}

// AgentRecall cites at most 3 ids and its block (whole lines only) never exceeds the byte budget.
func TestAgentRecallCappedAtThreeHitsAndByteBudget(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seedRecallBank(t, 5) // more candidates than the k=3 cap
	block, ids := AgentRecall("", t.TempDir(), recallTask)
	if len(ids) == 0 || len(ids) > 3 || strings.Count(block, "\n- ") != len(ids) {
		t.Errorf("ids=%v, want 1-3 matching the block's lines: %q", ids, block)
	}
	if len(block) > agentRecallBlockCapBytes || !strings.HasPrefix(block, "\n\n"+agentRecallMarker) {
		t.Errorf("block is %d bytes, want <= %d and marker-led: %q", len(block), agentRecallBlockCapBytes, block)
	}
}

// An empty cwd falls back to the process cwd: a home-bank hit still comes back, and a marked task returns nothing.
func TestAgentRecallHandlesEmptyCwd(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	seedRecallBank(t, 1)
	block, ids := AgentRecall("test-session", "", recallTask)
	if len(ids) != 1 || !strings.Contains(block, agentRecallMarker) {
		t.Errorf("empty cwd: ids=%v block=%q, want the one home-bank hit", ids, block)
	}
	if b, i := AgentRecall("test-session", "", recallTask+" "+agentRecallMarker); b != "" || i != nil {
		t.Errorf("a task already carrying the marker must return nothing, got %q %v", b, i)
	}
}
