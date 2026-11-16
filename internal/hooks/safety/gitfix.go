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

// gitFix fixes a footer commit or `;` push after a gate: a rewrite on Claude/Codex, else a deny with the command.
func gitFix(in *core.Input) (core.Result, bool) {
	if gitFixDisabled {
		return core.Result{}, false
	}
	if in.ToolName != "" && !strings.EqualFold(in.ToolName, "Bash") {
		return core.Result{}, false
	}
	cmd := in.ToolCommand()
	fixed, note := fixGitPolicy(core.DropDataHeredocs(cmd), cmd)
	if fixed == cmd {
		return core.Result{}, false
	}
	switch in.Agent() {
	case core.AgentClaude, core.AgentCodex:
		fin, ok := withCommand(in, fixed)
		if !ok {
			return core.Result{}, false
		}
		if d, _, _ := safetyVerdict(fin); d != core.Allow {
			return core.Result{}, false
		}
	}
	updated := in.CloneToolInput()
	if updated == nil || in.Nested("permission_mode") == "auto" || (in.Agent() != core.AgentClaude && in.Agent() != core.AgentCodex) {
		return core.Result{Decision: core.Block, Reason: "safety: " + note + "; resend exactly: " + fixed}, true
	}
	updated["command"] = fixed
	return core.Result{UpdatedInput: updated, SystemMessage: "safety: " + note}, true
}

// fixGitPolicy applies both corrections to cmd; data is cmd with data-only heredocs dropped, for the push check.
func fixGitPolicy(data, cmd string) (fixed, note string) {
	fixed = cmd
	if shell.AttributionFooterCommit(fixed) {
		if s, ok := shell.StripFooters(fixed); ok {
			fixed, note = s, "removed the AI-attribution footer from the commit message"
		}
	}
	if shell.PushAfterGate(data) {
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
		return nil, false
	}
	ti["command"] = cmd
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return core.ParseInputFor(strings.NewReader(string(raw)), in.Agent()), true
}
