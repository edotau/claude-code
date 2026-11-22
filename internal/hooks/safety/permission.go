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
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/shell"
)

// gateTools maps a canonical hook tool name (codex's apply_patch is Edit) to the rule tool Decide judges it as.
var gateTools = map[string]string{
	"bash": "Bash", "run_command": "Bash", "write": "Edit", "edit": "Edit", "multiedit": "Edit", "read": "Read",
	"write_file": "Edit", "edit_file": "Edit", "read_file": "Read", "webfetch": "WebFetch",
	"glob": "Glob", "grep": "Grep",
}

// PermissionGate blocks a tool call the dispatch's Deny rules (or plan mode) refuse; with no policy in env it passes.
func PermissionGate(in *core.Input) core.Result {
	r := permissionGate(in)
	r.Floor = r.Decision == core.Block // a dispatch's deny is the hard boundary: no leaf loosening allows it
	return r
}

func permissionGate(in *core.Input) core.Result {
	p, set, err := dispatchPolicy()
	if err != nil {
		return core.Result{Decision: core.Block, Reason: fmt.Sprintf(
			"permission-gate: BLOCKED — %s is unreadable (%v); every tool call is refused", agent.DispatchPermissionsEnv, err)}
	}
	if r := protectedEdit(in); r.Decision == core.Block {
		return r
	}
	if !set {
		return core.Result{}
	}
	if in.ToolName == "" && (len(p.Deny) > 0 || p.Mode == "plan") { // unparseable or truncated stdin: cannot be judged
		return noToolResult(p, in)
	}
	tool, ok := gateTools[strings.ToLower(in.CanonicalTool())]
	if !ok {
		return core.Result{}
	}
	return judgeCall(p, in.CWD, tool, gateArgs(in, tool))
}

// protectedEdit enforces settings deny and dispatch-decided ask globs on file edits, including apply_patch.
func protectedEdit(in *core.Input) core.Result {
	if gateTools[strings.ToLower(in.CanonicalTool())] != "Edit" {
		return core.Result{}
	}
	p := livePolicy()
	globs := foldedGlobs(append(extraGlobs(isLeaf(in)), p.editDeny...), in.CWD)
	asks := foldedGlobs(p.editAsk, in.CWD)
	for _, form := range gateArgs(in, "Edit") { // pathForms already hold the symlink-resolved spelling
		f := paths.ExpandHome(form)
		if !filepath.IsAbs(f) {
			continue
		}
		if editProtected(in.CWD, f, form, globs) && !tempWriteAllowed("", f) {
			return core.Result{Decision: core.Block, Floor: true, Reason: core.SafetyMessage(in, core.Block,
				"writes a file Edit/Write may never touch", f, "Edit it by hand, or run it yourself in a terminal.")}
		}
		if matchesProtected(f, asks) {
			d, msg := dispatchAsk(in, askHit{
				"edits a file settings.json asks before editing", "Edit", f, "Edit(" + f + ")",
			})
			if d != core.Allow {
				return core.Result{Decision: d, Reason: msg}
			}
		}
	}
	return core.Result{}
}

// judgeCall refuses the first of args the policy denies as tool, or whose unknown target a scoped deny may cover.
func judgeCall(p permission.Permissions, cwd, tool string, args []string) core.Result {
	if tool == "Bash" {
		p = readOnlyCDPolicy(p, args[0])
	}
	judge := anchorAllow(p, cwd)
	if tool != "Bash" && tool != "Edit" {
		judge = permission.Permissions{Deny: p.Deny} // plan mode reads through: only a Deny rule refuses a read
	}
	hasAbs := slices.ContainsFunc(args, filepath.IsAbs)
	for _, arg := range args {
		if r, blind := blindDeny(p, tool, arg); blind {
			return blindResult(p, tool, r)
		}
		j := judge
		// the cwd-relative spelling takes Deny rules only; its absolute forms meet the allow list
		if tool == "Edit" && hasAbs && !filepath.IsAbs(arg) {
			j = permission.Permissions{Deny: p.Deny}
		}
		if decideCall(j, cwd, tool, arg) == permission.Deny {
			return denyResult(p, cwd, permission.Rule{Tool: tool, Arg: arg})
		}
	}
	return core.Result{}
}

func readOnlyCDPolicy(p permission.Permissions, cmd string) permission.Permissions {
	if !slices.Contains(p.Deny, permission.ReadOnlyDeny[0]) {
		return p
	}
	cd := permission.Permissions{Mode: "plan", Allow: []permission.Rule{permission.CdRule("/")}}
	for _, seg := range shell.Segments(cmd) {
		if !permission.ReadOnlyShell(seg) && cd.Decide("Bash", seg) != permission.Allow {
			return p
		}
	}
	p.Deny = slices.DeleteFunc(slices.Clone(p.Deny), func(r permission.Rule) bool { return r == permission.ReadOnlyDeny[0] })
	return p
}

// GateBlocks reports whether the permission-gate refuses tool(arg) under p, for the doctor permissions report.
func GateBlocks(p permission.Permissions, tool, arg string) bool {
	return judgeCall(p, "", tool, []string{arg}).Decision == core.Block
}

// anchorAllow adds each relative Edit/Write allow rule anchored at an absolute cwd, so an absolute path meets it too.
func anchorAllow(p permission.Permissions, cwd string) permission.Permissions {
	if !filepath.IsAbs(cwd) {
		return p
	}
	allow := slices.Clone(p.Allow)
	for _, r := range p.Allow {
		if (r.Tool == "Edit" || r.Tool == "Write") && r.Arg != "" && !filepath.IsAbs(r.Arg) && !strings.HasPrefix(r.Arg, "~") {
			allow = append(allow, permission.Rule{Tool: r.Tool, Arg: filepath.ToSlash(filepath.Join(cwd, r.Arg))})
		}
	}
	p.Allow = allow
	return p
}

// decideCall judges arg as tool run in cwd, plus the tool that also covers it: Edit is Write, Grep and Glob are Read.
func decideCall(judge permission.Permissions, cwd, tool, arg string) permission.Decision {
	d := judge.DecideAt(cwd, tool, arg)
	switch tool {
	case "Edit":
		d = min(d, judge.Decide("Write", arg)) // opencode's edit permission covers both, so either rule refuses
	case "Grep", "Glob":
		d = min(d, judge.Decide("Read", arg))
	}
	return d
}

// dispatchPolicy is the dispatch's Permissions from env; set is false when none is exported.
func dispatchPolicy() (p permission.Permissions, set bool, err error) {
	raw := strings.TrimSpace(os.Getenv(agent.DispatchPermissionsEnv))
	if raw == "" {
		return p, false, nil
	}
	err = json.Unmarshal([]byte(raw), &p)
	return p, true, err
}

// noToolResult refuses a call whose hook input named no tool, under a policy with Deny rules or plan mode.
func noToolResult(p permission.Permissions, in *core.Input) core.Result {
	why := cmp.Or(in.Invalid, "missing tool_name in hook payload")
	if in.Invalid == "" && len(strings.TrimSpace(string(in.Raw))) == 0 {
		why = "empty hook payload"
	}
	return core.Result{Decision: core.Block, Reason: "permission-gate: BLOCKED — the hook input names no tool (" + why +
		"), so the call cannot be judged\n  deny: " + strings.Join(permission.RuleStrings(p.Deny), ", ") +
		modeNote(p) + "\nReport it instead of retrying another way."}
}

// blindDeny refuses a Read/Edit whose target is unknown (no path key, no patch header) when a scoped Deny covers it.
func blindDeny(p permission.Permissions, tool, arg string) (permission.Rule, bool) {
	if arg != "" || pathlessTools[tool] {
		return permission.Rule{}, false
	}
	for _, r := range p.Deny {
		if r.Arg != "" && (r.Tool == tool || tool == "Edit" && r.Tool == "Write") {
			return r, true
		}
	}
	return permission.Rule{}, false
}

// pathlessTools never name an unknown target without a path: a command, a URL, or (Grep/Glob) the cwd.
var pathlessTools = map[string]bool{"Bash": true, "WebFetch": true, "Grep": true, "Glob": true}

// blindResult refuses a pathless call, naming the tool and the scoped deny that made its unknown target unsafe.
func blindResult(p permission.Permissions, tool string, rule permission.Rule) core.Result {
	return core.Result{Decision: core.Block, Reason: "permission-gate: BLOCKED — " + tool + " call has no path, so its " +
		"target is unknown and may fall under the scoped deny " + rule.String() + "\n  deny: " +
		strings.Join(permission.RuleStrings(p.Deny), ", ") + modeNote(p) +
		"\nPass an explicit path outside the denied scope, or report it."}
}

func explainDeniedCall(p permission.Permissions, cwd string, call permission.Rule) permission.Denial {
	d := p.ExplainAt(cwd, call.Tool, call.Arg)
	alias := ""
	switch call.Tool {
	case "Edit":
		alias = "Write"
	case "Grep", "Glob":
		alias = "Read"
	}
	if alias != "" {
		if other := p.ExplainAt(cwd, alias, call.Arg); other.Source == "permissions.deny" {
			d = other
		}
	}
	return d
}

func denyResult(p permission.Permissions, cwd string, call permission.Rule) core.Result {
	d := explainDeniedCall(p, cwd, call)
	rules := "\n  deny: " + strings.Join(permission.RuleStrings(p.Deny), ", ")
	next := "\nReport it instead of retrying another way."
	if seg := deniedSegment(p, call); seg != "" {
		rules += "\n  denied segment: " + seg
	}
	switch {
	case call.Tool == "Bash" && strings.Contains(call.Arg, ">") && anyDenyArg(p.Deny, permission.RedirectFileArg):
		next = "\nFile redirects (`>`, `>>`, `&>`) are refused; `2>&1` is allowed: run the same command again without them."
	case p.Mode == "plan" && len(p.Allow) > 0: // an allow-list miss is not a reason to abandon the job
		rules += "\n  allow: " + strings.Join(permission.RuleStrings(p.Allow), ", ")
		next = "\nOnly the allow list runs here: " + planMissHint(p, call)
		if d.Replay != "" {
			next += "\nOnly the dispatcher can permit it: re-dispatch with " + d.Remedy + "."
		} else if d.Source != "permissions.deny" {
			next += "\n" + d.Remedy + "."
		}
	case call.Tool == "Bash" && d.Rule == permission.ReadOnlyDeny[0]: // read-only is an allow list spelled as a deny
		next = "\nOnly read-only commands run here: drop the denied segment or rewrite it with an allowed command " +
			"(static absolute paths for cd)."
	}
	if d.Source != "" {
		rules += "\n  layer: " + d.Layer + " source: " + d.Source
		if d.Rule.Tool != "" {
			rules += " rule: " + d.Rule.String()
		}
	}
	return core.Result{Decision: core.Block, Reason: "permission-gate: BLOCKED — the dispatch's permission rules deny " +
		call.String() + rules + modeNote(p) + next}
}

// planMissHint names what missed the allow list and the closest allowed form of the same command or path, if any.
func planMissHint(p permission.Permissions, call permission.Rule) string {
	seg := cmp.Or(deniedSegment(p, call), strings.TrimSpace(call.Arg))
	hint := call.Tool + "(" + seg + ") is not on it: "
	if near, ok := nearestAllow(p.Allow, call.Tool, seg); ok {
		hint += "the nearest allowed form is " + near.String() + "; rewrite the call to fit it or "
	}
	return hint + "continue with another allowed call."
}

// nearestAllow is the rule for tool sharing the longest prefix with arg, past its first word or path element.
func nearestAllow(rules []permission.Rule, tool, arg string) (permission.Rule, bool) {
	var best permission.Rule
	bestLen := 0
	for _, r := range rules {
		if r.Arg == "" || r.Tool != tool && (tool != "Edit" || r.Tool != "Write") {
			continue
		}
		form := strings.Replace(r.Arg, "cd:", "cd ", 1)
		if n := commonPrefix(form, arg); n > bestLen && n > firstWordEnd(arg) {
			best, bestLen = r, n
		}
	}
	return best, bestLen > 0
}

// firstWordEnd is the length of arg's first word, or of its first element for an absolute path.
func firstWordEnd(arg string) int {
	if i := strings.IndexAny(strings.TrimPrefix(arg, "/"), " /"); i >= 0 {
		return i + len(arg) - len(strings.TrimPrefix(arg, "/"))
	}
	return len(arg) - 1 // a one-word call needs the whole word in common
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// deniedSegment is the first segment of a compound Bash call that p denies on its own; "" otherwise.
func deniedSegment(p permission.Permissions, call permission.Rule) string {
	if call.Tool != "Bash" {
		return ""
	}
	segs := shell.Segments(call.Arg)
	for _, seg := range segs {
		if seg = strings.TrimSpace(seg); len(segs) > 1 && p.Decide("Bash", seg) == permission.Deny {
			return seg
		}
	}
	return ""
}

func anyDenyArg(rules []permission.Rule, arg string) bool {
	return slices.ContainsFunc(rules, func(r permission.Rule) bool { return r.Arg == arg })
}

func modeNote(p permission.Permissions) string {
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
	var out []string
	for _, e := range in.EditedPaths() {
		out = append(out, pathForms(in.CWD, e.Path)...)
	}
	if len(out) > 0 {
		return out
	}
	return pathForms(in.CWD, "")
}

// pathForms are p's cwd-relative and absolute spellings, as typed and symlink-resolved; "" stays "".
func pathForms(cwd, p string) []string {
	if p == "" {
		return []string{""}
	}
	abs := paths.AnchorPath(p, cwd)
	forms := []string{relToCWD(cwd, p), abs}
	if filepath.IsAbs(abs) {
		canonCWD := cwd
		if cwd != "" {
			canonCWD = paths.Real(cwd)
		}
		canon := paths.Real(abs)
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
