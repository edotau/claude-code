// permission-gate — a dispatch's Permissions (HARNESS_DISPATCH_PERMISSIONS) judged per tool call inside the agent.

package safety

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
)

// gateTools maps a hook tool name, Claude's or opencode's, to the rule tool Decide judges it as.
var gateTools = map[string]string{
	"bash": "Bash", "write": "Edit", "edit": "Edit", "multiedit": "Edit", "read": "Read", "webfetch": "WebFetch",
}

// PermissionGate blocks a tool call the dispatch's Deny rules (or plan mode) refuse; with no policy in env it passes.
func PermissionGate(in *core.Input) core.Result {
	raw := strings.TrimSpace(os.Getenv(agent.DispatchPermissionsEnv))
	if raw == "" {
		return core.Result{}
	}
	var p agent.Permissions
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return core.Result{Decision: core.Block, Reason: fmt.Sprintf(
			"permission-gate: BLOCKED — %s is unreadable (%v); every tool call is refused", agent.DispatchPermissionsEnv, err)}
	}
	tool, ok := gateTools[strings.ToLower(in.ToolName)]
	if !ok {
		return core.Result{}
	}
	arg := gateArg(in, tool)
	judge := p
	if tool == "Read" || tool == "WebFetch" {
		judge = agent.Permissions{Deny: p.Deny} // plan mode reads through: only a Deny rule refuses a read
	}
	d := judge.Decide(tool, arg)
	if tool == "Edit" {
		d = min(d, judge.Decide("Write", arg)) // opencode's edit permission covers both, so either rule refuses
	}
	if d != agent.Deny {
		return core.Result{}
	}
	call := agent.Rule{Tool: tool, Arg: arg}
	return core.Result{Decision: core.Block, Reason: "permission-gate: BLOCKED — the dispatch's permission rules deny " +
		call.String() + "\n  deny: " + strings.Join(agent.RuleStrings(p.Deny), ", ") + modeNote(p) +
		"\nReport it instead of retrying another way."}
}

func modeNote(p agent.Permissions) string {
	if p.Mode == "" {
		return ""
	}
	return "\n  mode: " + p.Mode
}

// gateArg is the argument a rule matches for tool: the command, a cwd-relative path, or the URL.
func gateArg(in *core.Input, tool string) string {
	switch tool {
	case "Bash":
		return in.ToolCommand()
	case "WebFetch":
		return in.Nested("tool_input", "url")
	}
	for _, key := range []string{"file_path", "filePath", "path"} {
		if p := in.Nested("tool_input", key); p != "" {
			return relToCWD(in.CWD, p)
		}
	}
	return ""
}

// relToCWD is p relative to cwd when inside it, else p unchanged (copilot's relToWorkDir).
func relToCWD(cwd, p string) string {
	if rel, err := filepath.Rel(cwd, p); cwd != "" && filepath.IsAbs(p) && err == nil && filepath.IsLocal(rel) {
		return filepath.ToSlash(rel)
	}
	return p
}
