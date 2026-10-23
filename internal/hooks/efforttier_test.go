package hooks

import "testing"

// TestEffortTierSignalRules pins each classifier rule to its tier AND the signal name the hook
// surfaces in systemMessage — a tier without a matching signal name is a silent regression nobody
// watching the message would ever notice.
func TestEffortTierSignalRules(t *testing.T) {
	cases := []struct {
		name, prompt, wantTier, wantSignal string
	}{
		{"open search: investigate", "investigate why the build is flaky", "high", "open-search"},
		{"open search: track down", "track down the memory leak", "high", "open-search"},
		{"open search: root cause", "find the root cause of the crash", "high", "open-search"},
		{"open search: debug", "debug the failing request", "high", "open-search"},
		{"live system: production", "check the production deploy logs", "high", "live-system"},
		{"live system: cluster", "restart the cluster node", "high", "live-system"},
		{"live system: database", "check why the database is slow right now", "high", "live-system"},
		{"single path mechanical: low", "rename the var in internal/foo.go", "low", "mechanical"},
		{"single path mechanical, scope word: neutral", "rename the var across internal/foo.go and everywhere", "medium", "neutral"},
		{"mechanical, no path, no scope: low", "fix the typo in the help text", "low", "mechanical"},
		{"scope word alone keeps neutral", "rename it everywhere in the repo", "medium", "neutral"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tier, signal := EffortTierSignal(c.prompt)
			if tier != c.wantTier || signal != c.wantSignal {
				t.Errorf("EffortTierSignal(%q) = (%q, %q), want (%q, %q)", c.prompt, tier, signal, c.wantTier, c.wantSignal)
			}
		})
	}
}

// TestEffortTierSignalHighRulePriority: an explicit high-complexity keyword wins even when a prompt
// would otherwise also match open-search or live-system, and names the specific keyword that fired.
func TestEffortTierSignalHighRulePriority(t *testing.T) {
	tier, signal := EffortTierSignal("refactor the production deploy pipeline and investigate the outage")
	if tier != "high" {
		t.Fatalf("tier = %q, want high", tier)
	}
	if signal != "keyword:refactor" {
		t.Errorf("signal = %q, want keyword:refactor (the first matched highRe word)", signal)
	}
}
