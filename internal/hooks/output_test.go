package hooks

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hooks/core"
)

func oneDoc(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("not JSON: %q: %v", s, err)
	}
	if dec.More() {
		t.Fatalf("more than one JSON document: %q", s)
	}
	return m
}

func TestOutputClaudeShape(t *testing.T) {
	m := oneDoc(t, core.OutputClaude("PreToolUse", core.Result{Decision: core.Block, Reason: "r", Context: "c", SystemMessage: "s"}))
	h, _ := m["hookSpecificOutput"].(map[string]any)
	if h["hookEventName"] != "PreToolUse" || h["permissionDecision"] != "deny" || h["permissionDecisionReason"] != "r" || h["additionalContext"] != "c" || m["systemMessage"] != "s" {
		t.Errorf("shape: %v", m)
	}
	if core.OutputClaude("PreToolUse", core.Result{}) != "" {
		t.Error("empty result must print nothing")
	}
}

func TestOutputCodexSingleDocument(t *testing.T) {
	m := oneDoc(t, core.OutputCodex("PreToolUse", core.Result{Decision: core.Ask, Reason: "r", Context: "c1\n\nc2", SystemMessage: "a; b"}))
	if _, ok := m["hookSpecificOutput"]; !ok {
		t.Errorf("codex uses the claude-compatible schema: %v", m)
	}
}

func TestOutputCopilotAskIsDeny(t *testing.T) {
	m := oneDoc(t, core.OutputCopilot("PreToolUse", core.Result{Decision: core.Ask, Reason: "r", Context: "c"}))
	if m["permissionDecision"] != "deny" || m["permissionDecisionReason"] != "r" || m["additionalContext"] != "c" {
		t.Errorf("ask must map to deny: %v", m)
	}
}

func TestOutputEmitBlockOutsidePreToolUse(t *testing.T) {
	var out, errw bytes.Buffer
	code := core.Emit(core.AgentClaude, "Stop", core.Result{Decision: core.Block, Reason: "keep going"}, &out, &errw)
	if code != core.ExitBlock || !strings.Contains(errw.String(), "keep going") || out.Len() != 0 {
		t.Errorf("code=%d stderr=%q stdout=%q", code, errw.String(), out.String())
	}
	out.Reset()
	errw.Reset()
	if code := core.Emit(core.AgentClaude, "PreToolUse", core.Result{Decision: core.Block, Reason: "r"}, &out, &errw); code != core.ExitProceed || !strings.Contains(out.String(), "deny") {
		t.Errorf("PreToolUse block rides JSON: code=%d out=%q", code, out.String())
	}
}
