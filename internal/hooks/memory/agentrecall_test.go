package memory

import (
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
	if len(block) > AgentRecallBlockCapBytes {
		t.Errorf("block is %d bytes, want <= %d", len(block), AgentRecallBlockCapBytes)
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
