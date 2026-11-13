// permission-gate — a dispatch's Permissions (HARNESS_DISPATCH_PERMISSIONS) judged per tool call inside the agent.

package safety

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
)

// gateTools maps a canonical hook tool name (codex's apply_patch is Edit) to the rule tool Decide judges it as.
var gateTools = map[string]string{
	"bash": "Bash", "run_command": "Bash", "write": "Edit", "edit": "Edit", "multiedit": "Edit", "read": "Read",
	"write_file": "Edit", "edit_file": "Edit", "read_file": "Read", "webfetch": "WebFetch",
	"glob": "Glob", "grep": "Grep",
}

// PermissionGate blocks a tool call the dispatch's Deny rules (or plan mode) refuse; with no policy in env it passes.
func PermissionGate(in *core.Input) core.Result {
	p, set, err := dispatchPolicy()
	if err != nil {
		return core.Result{Decision: core.Block, Reason: fmt.Sprintf(
			"permission-gate: BLOCKED — %s is unreadable (%v); every tool call is refused", agent.DispatchPermissionsEnv, err)}
	}
	if !set {
		return core.Result{}
	}
	if in.ToolName == "" && (len(p.Deny) > 0 || p.Mode == "plan") { // unparseable or truncated stdin: cannot be judged
		return noToolResult(p)
	}
	tool, ok := gateTools[strings.ToLower(in.CanonicalTool())]
	if !ok {
		return core.Result{}
	}
	return judgeCall(p, tool, gateArgs(in, tool))
}

// judgeCall refuses the first of args the policy denies as tool, or whose unknown target a scoped deny may cover.
func judgeCall(p agent.Permissions, tool string, args []string) core.Result {
	judge := p
	if tool != "Bash" && tool != "Edit" {
		judge = agent.Permissions{Deny: p.Deny} // plan mode reads through: only a Deny rule refuses a read
	}
	hasAbs := slices.ContainsFunc(args, filepath.IsAbs)
	for _, arg := range args {
		if r, blind := blindDeny(p, tool, arg); blind {
			return blindResult(p, tool, r)
		}
		j := judge
		if tool == "Edit" && hasAbs && !filepath.IsAbs(arg) { // the cwd-relative spelling: Deny rules only, its absolute forms meet the allow list
			j = agent.Permissions{Deny: p.Deny}
		}
		if decideCall(j, tool, arg) == agent.Deny {
			return denyResult(p, agent.Rule{Tool: tool, Arg: arg})
		}
	}
	return core.Result{}
}

// decideCall judges arg as tool plus the rule tool that also covers it: Edit is Write too, Grep and Glob are Read too.
func decideCall(judge agent.Permissions, tool, arg string) agent.Decision {
	d := judge.Decide(tool, arg)
	switch tool {
	case "Edit":
		d = min(d, judge.Decide("Write", arg)) // opencode's edit permission covers both, so either rule refuses
	case "Grep", "Glob":
		d = min(d, judge.Decide("Read", arg))
	}
	return d
}

// dispatchPolicy is the dispatch's Permissions from env; set is false when none is exported.
func dispatchPolicy() (p agent.Permissions, set bool, err error) {
	raw := strings.TrimSpace(os.Getenv(agent.DispatchPermissionsEnv))
	if raw == "" {
		return p, false, nil
	}
	err = json.Unmarshal([]byte(raw), &p)
	return p, true, err
}

// noToolResult refuses a call whose hook input named no tool, under a policy with Deny rules or plan mode.
func noToolResult(p agent.Permissions) core.Result {
	return core.Result{Decision: core.Block, Reason: "permission-gate: BLOCKED — the hook input names no tool (unreadable " +
		"or truncated payload), so the call cannot be judged\n  deny: " + strings.Join(agent.RuleStrings(p.Deny), ", ") +
		modeNote(p) + "\nReport it instead of retrying another way."}
}

// blindDeny refuses a Read/Edit whose target is unknown (no path key, no patch header) when a scoped Deny covers it.
func blindDeny(p agent.Permissions, tool, arg string) (agent.Rule, bool) {
	if arg != "" || pathlessTools[tool] {
		return agent.Rule{}, false
	}
	for _, r := range p.Deny {
		if r.Arg != "" && (r.Tool == tool || tool == "Edit" && r.Tool == "Write") {
			return r, true
		}
	}
	return agent.Rule{}, false
}

// pathlessTools never name an unknown target without a path: a command, a URL, or (Grep/Glob) the cwd.
var pathlessTools = map[string]bool{"Bash": true, "WebFetch": true, "Grep": true, "Glob": true}

// blindResult refuses a pathless call, naming the tool and the scoped deny that made its unknown target unsafe.
func blindResult(p agent.Permissions, tool string, rule agent.Rule) core.Result {
	return core.Result{Decision: core.Block, Reason: "permission-gate: BLOCKED — " + tool + " call has no path, so its " +
		"target is unknown and may fall under the scoped deny " + rule.String() + "\n  deny: " +
		strings.Join(agent.RuleStrings(p.Deny), ", ") + modeNote(p) +
		"\nPass an explicit path outside the denied scope, or report it."}
}

func denyResult(p agent.Permissions, call agent.Rule) core.Result {
	rules := "\n  deny: " + strings.Join(agent.RuleStrings(p.Deny), ", ")
	next := "\nReport it instead of retrying another way."
	switch {
	case call.Tool == "Bash" && strings.Contains(call.Arg, ">") && anyDenyArg(p.Deny, "*>*"):
		next = "\nRedirects (`>`, `2>&1`) are refused: run the same command again without them."
	case p.Mode == "plan" && len(p.Allow) > 0: // an allow-list miss is not a reason to abandon the job
		rules += "\n  allow: " + strings.Join(agent.RuleStrings(p.Allow), ", ")
		next = "\nOnly the allow list runs here: continue with an allowed call; do not retry this one another way."
	}
	return core.Result{Decision: core.Block, Reason: "permission-gate: BLOCKED — the dispatch's permission rules deny " +
		call.String() + rules + modeNote(p) + next}
}

func anyDenyArg(rules []agent.Rule, arg string) bool {
	return slices.ContainsFunc(rules, func(r agent.Rule) bool { return r.Arg == arg })
}

func modeNote(p agent.Permissions) string {
	if p.Mode == "" {
		return ""
	}
	return "\n  mode: " + p.Mode
}

// gateArgs are what a rule matches for tool: the command, the URL, or the paths, one per file an apply_patch touches.
func gateArgs(in *core.Input, tool string) []string {
	switch tool {
	case "Bash":
		return []string{in.ToolCommand()}
	case "WebFetch":
		return []string{in.Nested("tool_input", "url")}
	}
	for _, key := range patchKeys {
		var out []string
		for _, f := range patchFiles(in.Nested("tool_input", key)) {
			out = append(out, pathForms(in.CWD, f)...)
		}
		if len(out) > 0 {
			return out
		}
	}
	return pathForms(in.CWD, cmp.Or(in.Nested("tool_input", "file_path"), in.Nested("tool_input", "filePath"),
		in.Nested("tool_input", "path")))
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

// patchKeys are the tool_input keys a patch body arrives under: opencode's, then codex's.
var patchKeys = []string{"patchText", "patch", "input", "command"}

var patchHeaders = []string{"*** Add File:", "*** Update File:", "*** Delete File:", "*** Move to:"}

// pathForms are p's cwd-relative and absolute spellings, as typed and symlink-resolved; "" stays "".
func pathForms(cwd, p string) []string {
	if p == "" {
		return []string{""}
	}
	abs := core.AnchorPath(p, cwd)
	forms := []string{relToCWD(cwd, p), abs}
	if filepath.IsAbs(abs) {
		canonCWD := cwd
		if cwd != "" {
			canonCWD = core.CanonPath(cwd)
		}
		canon := core.CanonPath(abs)
		forms = append(forms, relToCWD(canonCWD, canon), canon)
	}
	var out []string
	for _, f := range forms {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// relToCWD is p relative to cwd when inside it, else p unchanged (copilot's relToWorkDir).
func relToCWD(cwd, p string) string {
	if rel, err := filepath.Rel(cwd, p); cwd != "" && filepath.IsAbs(p) && err == nil && filepath.IsLocal(rel) {
		return filepath.ToSlash(rel)
	}
	return p
}
