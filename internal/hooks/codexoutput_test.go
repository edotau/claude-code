package hooks

import (
	"testing"
)

// Ruling 4: Codex has no ask decision — ask must render as deny. oneDoc also proves a single JSON document.
func TestOutputCodexAskMapsToDeny(t *testing.T) {
	m := oneDoc(t, OutputCodex("PreToolUse", Result{Decision: Ask, Reason: "needs review"}))
	h, _ := m["hookSpecificOutput"].(map[string]any)
	if h["permissionDecision"] != "deny" || h["permissionDecisionReason"] != "needs review" {
		t.Errorf("ask must map to deny: %v", m)
	}
}

// The wire shape codex-agent-route emits today: an updatedInput on PreToolUse is an explicit allow.
func TestOutputCodexAllowCarriesUpdatedInput(t *testing.T) {
	m := oneDoc(t, OutputCodex("PreToolUse", Result{UpdatedInput: map[string]any{"model": "m", "reasoning_effort": "high"}}))
	h, _ := m["hookSpecificOutput"].(map[string]any)
	if h["permissionDecision"] != "allow" {
		t.Errorf("updatedInput on PreToolUse must be an explicit allow: %v", m)
	}
	got, _ := h["updatedInput"].(map[string]any)
	if got["model"] != "m" || got["reasoning_effort"] != "high" {
		t.Errorf("updatedInput = %v", got)
	}
}

func TestOutputCodexBlockDeniesWithReason(t *testing.T) {
	m := oneDoc(t, OutputCodex("PreToolUse", Result{Decision: Block, Reason: "no"}))
	h, _ := m["hookSpecificOutput"].(map[string]any)
	if h["permissionDecision"] != "deny" || h["permissionDecisionReason"] != "no" {
		t.Errorf("block must deny with reason: %v", m)
	}
}

// updatedInput is a PreToolUse-only concept: outside that event it must not leak a permissionDecision.
func TestOutputCodexUpdatedInputIgnoredOutsidePreToolUse(t *testing.T) {
	if out := OutputCodex("PostToolUse", Result{UpdatedInput: map[string]any{"model": "m"}}); out != "" {
		t.Errorf("non-PreToolUse updatedInput must render nothing, got %q", out)
	}
	m := oneDoc(t, OutputCodex("PostToolUse", Result{UpdatedInput: map[string]any{"model": "m"}, Context: "c"}))
	h, _ := m["hookSpecificOutput"].(map[string]any)
	if _, ok := h["permissionDecision"]; ok {
		t.Errorf("permissionDecision outside PreToolUse: %v", m)
	}
	if _, ok := h["updatedInput"]; ok {
		t.Errorf("updatedInput outside PreToolUse: %v", m)
	}
}
