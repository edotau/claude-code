package hooks

import (
	"testing"

	"github.com/edotau/claude-code/internal/memory"
)

func fxHit(id string, score float64, matched int) memory.SearchHit {
	return memory.SearchHit{Score: score, Matched: matched, Bank: "home", File: "decisionLog.md",
		Block: "## 2026-10-01 — Zebra quota " + id + " <!-- id:" + id + " -->\n\nzebra quota limiter fallback"}
}

// A boosted top hit must not raise the relative floor: a hit at half the raw top still survives.
func TestFxAccessBoostAppliedAfterFloor(t *testing.T) {
	ranked := []memory.SearchHit{fxHit("aa000001", 10, 3), fxHit("cc000003", 5, 3)}
	picks, _ := pickTier(ranked, false, 3, nil, nil, map[string]int{"aa000001": recallAccessCap})
	if len(picks) != 2 {
		t.Fatalf("the boost lifted the floor over a qualifying hit: %+v", picks)
	}
}
