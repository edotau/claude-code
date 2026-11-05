package opencode

import "testing"

// TestProvisionalSessionIsUniquePerDispatch pins that two dispatches in the same second never share hook state.
func TestProvisionalSessionIsUniquePerDispatch(t *testing.T) {
	a, b := provisionalSession(), provisionalSession()
	if a == b {
		t.Errorf("back-to-back provisional ids are equal (%q): concurrent dispatches would share hook state", a)
	}
}

// TestHostedSetComesFromRegistry pins the registry's opencode hosted set to the modules the host was built around.
func TestHostedSetComesFromRegistry(t *testing.T) {
	want := []string{"memory-start", "worktree-mode", "memory-recall", "paths-resolve", "docs-plan-skeleton-prompt",
		"worktree-busy", "safety-scan", "docs-placement", "docs-plan-gate", "docs-plan-mirror", "review-simplicity",
		"docs-plan-tick", "memory-update", "format-stop", "docs-plan-reminder", "memory-end"}
	got := hostedSet()
	if len(got) != len(want) {
		t.Fatalf("hosted set has %d modules, want %d: %v", len(got), len(want), got)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("hosted set is missing %s", name)
		}
	}
}
