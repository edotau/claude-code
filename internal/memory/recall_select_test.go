package memory

import "testing"

// Access boosts break ties without admitting entries that fail relevance gates.
func TestPickTierAccessBoost(t *testing.T) {
	hit := func(id string, score float64, matched int) SearchHit {
		return SearchHit{Score: score, Matched: matched, Bank: "home", File: "decisionLog.md",
			Block: "## 2026-10-01 — Zebra quota " + id + " <!-- id:" + id + " -->\n\nzebra quota limiter fallback"}
	}
	ranked := []SearchHit{hit("aa000001", 10, 3), hit("bb000002", 10, 3)}
	picks, passed := pickTier(ranked, false, 3, nil, nil, map[string]int{"bb000002": 3}, nil)
	if !passed || len(picks) != 2 || picks[0].Entry.ID != "bb000002" {
		t.Fatalf("the returned-to entry must rank first: %+v (passed %v)", picks, passed)
	}
	picks, _ = pickTier(ranked, false, 3, nil, nil, nil, nil)
	if len(picks) != 2 || picks[0].Entry.ID != "aa000001" {
		t.Fatalf("without returns the stable keyword order must hold: %+v", picks)
	}

	weak := []SearchHit{hit("aa000001", 10, 3), hit("cc000003", 2, 1)}
	picks, _ = pickTier(weak, false, 3, nil, nil, map[string]int{"cc000003": recallAccessCap}, nil)
	if len(picks) != 1 || picks[0].Entry.ID != "aa000001" {
		t.Fatalf("a returned-to entry failing the gate must not be admitted: %+v", picks)
	}

	// A boosted top hit must not raise the relative floor: a hit at half the raw top still survives.
	half := []SearchHit{hit("aa000001", 10, 3), hit("cc000003", 5, 3)}
	picks, _ = pickTier(half, false, 3, nil, nil, map[string]int{"aa000001": recallAccessCap}, nil)
	if len(picks) != 2 {
		t.Fatalf("the boost lifted the floor over a qualifying hit: %+v", picks)
	}
}
