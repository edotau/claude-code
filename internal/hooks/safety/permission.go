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

// gateTools maps a hook tool name, Claude's, opencode's or agy's, to the rule tool Decide judges it as.
var gateTools = map[string]string{
	"bash": "Bash", "run_command": "Bash", "write": "Edit", "edit": "Edit", "multiedit": "Edit", "read": "Read",
	"write_file": "Edit", "edit_file": "Edit", "read_file": "Read", "webfetch": "WebFetch",
	"glob": "Glob", "grep": "Grep",
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
	judge := p
	if tool != "Bash" && tool != "Edit" {
		judge = agent.Permissions{Deny: p.Deny} // plan mode reads through: only a Deny rule refuses a read
	}
	for _, arg := range gateArgs(in, tool) {
		if r, blind := blindDeny(p, tool, arg); blind {
			return blindResult(p, tool, r)
		}
		d := judge.Decide(tool, arg)
		if tool == "Edit" {
			d = min(d, judge.Decide("Write", arg)) // opencode's edit permission covers both, so either rule refuses
		}
		if d == agent.Deny {
			return denyResult(p, agent.Rule{Tool: tool, Arg: arg})
		}
	}
	return core.Result{}
}

// blindDeny refuses a Read/Edit whose target is unknown (no path key, no patch header) when a scoped Deny covers it.
func blindDeny(p agent.Permissions, tool, arg string) (agent.Rule, bool) {
	if arg != "" || tool == "Bash" || tool == "WebFetch" || tool == "Grep" || tool == "Glob" { // Grep/Glob default to cwd
		return agent.Rule{}, false
	}
	for _, r := range p.Deny {
		if r.Arg != "" && (r.Tool == tool || tool == "Edit" && r.Tool == "Write") {
			return r, true
		}
	}
	return agent.Rule{}, false
}

// blindResult refuses a pathless call, naming the tool and the scoped deny that made its unknown target unsafe.
func blindResult(p agent.Permissions, tool string, rule agent.Rule) core.Result {
	return core.Result{Decision: core.Block, Reason: "permission-gate: BLOCKED — " + tool + " call has no path, so its " +
		"target is unknown and may fall under the scoped deny " + rule.String() + "\n  deny: " +
		strings.Join(agent.RuleStrings(p.Deny), ", ") + modeNote(p) +
		"\nPass an explicit path outside the denied scope, or report it."}
}

func denyResult(p agent.Permissions, call agent.Rule) core.Result {
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

// gateArgs are the arguments a rule matches for tool: the command, the URL, or the cwd-relative paths — one per file an
// opencode apply_patch touches, each judged on its own.
func gateArgs(in *core.Input, tool string) []string {
	switch tool {
	case "Bash":
		return []string{in.ToolCommand()}
	case "WebFetch":
		return []string{in.Nested("tool_input", "url")}
	}
	if patch := in.Nested("tool_input", "patchText"); patch != "" {
		var out []string
		for _, f := range patchFiles(patch) {
			out = append(out, relToCWD(in.CWD, f))
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{gatePath(in)}
}

// patchFiles are the files an apply_patch body adds, updates, deletes or moves to.
func patchFiles(patch string) []string {
	var out []string
	for line := range strings.SplitSeq(patch, "\n") {
		for _, h := range patchHeaders {
			if f, ok := strings.CutPrefix(line, h); ok && strings.TrimSpace(f) != "" {
				out = append(out, strings.TrimSpace(f))
			}
		}
	}
	return out
}

var patchHeaders = []string{"*** Add File:", "*** Update File:", "*** Delete File:", "*** Move to:"}

func gatePath(in *core.Input) string {
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
