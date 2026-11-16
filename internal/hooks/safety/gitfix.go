package safety

import (
	"encoding/json"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

const commitUse = `use: git commit -m "fix(<scope>): <summary>"`

// gitFixDisabled holds rewrites off until the 2026-11-16 review's bypass findings are fixed; denies stand.
var gitFixDisabled = true

// gitFix rewrites a flagged footer commit or `;` push (deny with the command off Claude/Codex); the rewrite must pass.
func gitFix(in *core.Input, orig core.Decision) (core.Result, bool) {
	if gitFixDisabled {
		return core.Result{}, false
	}
	if orig == core.Allow || in.ToolName != "" && !strings.EqualFold(in.ToolName, "Bash") {
		return core.Result{}, false
	}
	cmd := in.ToolCommand()
	fixed, note := fixGitPolicy(cmd)
	if fixed == cmd {
		return core.Result{}, false
	}
	fin, ok := withCommand(in, fixed)
	if !ok {
		return core.Result{}, false
	}
	if d, _, _ := safetyVerdict(fin); d != core.Allow {
		return core.Result{}, false
	}
	updated := in.CloneToolInput()
	if updated == nil || (in.Agent() != core.AgentClaude && in.Agent() != core.AgentCodex) {
		return core.Result{Decision: core.Block, Reason: "safety: " + note + "; resend exactly: " + fixed}, true
	}
	updated["command"] = fixed
	return core.Result{UpdatedInput: updated, SystemMessage: "safety: " + note}, true
}

// fixGitPolicy applies both corrections to a simple cmd; unchanged when it is any other shape.
func fixGitPolicy(cmd string) (fixed, note string) {
	fixed = cmd
	if shell.AttributionFooterCommit(fixed) {
		if s, ok := shell.StripFooters(fixed); ok {
			fixed, note = s, "removed the AI-attribution footer from the commit message"
		}
	}
	if shell.PushAfterGate(fixed) {
		if s, gate, ok := shell.ChainPushAfterGate(fixed); ok {
			fixed, note = s, "chained git push with && after "+gate
		}
	}
	return fixed, note
}

// withCommand is in re-read with its tool_input command replaced.
func withCommand(in *core.Input, cmd string) (*core.Input, bool) {
	var doc map[string]any
	if json.Unmarshal(in.Raw, &doc) != nil {
		return nil, false
	}
	ti, _ := doc["tool_input"].(map[string]any)
	if ti == nil {
		ti, _ = doc["toolArgs"].(map[string]any) // Copilot
	}
	if ti == nil {
		return nil, false
	}
	ti["command"] = cmd
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return core.ParseInputFor(strings.NewReader(string(raw)), in.Agent()), true
}
