package safety

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/permission"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/settings"
)

// policy is the settings.json permissions safety-scan reads: Bash rules per tier, Edit/Write globs as written.
type policy struct {
	bashAllow, bashAsk, bashDeny []permission.Rule
	editAsk, editDeny            []string
}

var (
	policyCache    = paths.NewStampCache[policy]("safety.policy")
	templatePolicy = sync.OnceValue(func() policy {
		var doc struct {
			Permissions settings.PermissionLists `json:"permissions"`
		}
		_ = json.Unmarshal(settings.Template(), &doc) // the embedded template is tested
		return parsePolicy(doc.Permissions)
	})
)

// livePolicy is the user settings policy (D3); a missing or broken settings.json falls back to the template's.
func livePolicy() policy {
	p, err := policyCache.Get(settings.SettingsFile(), func() (policy, error) {
		live, err := settings.LivePermissions()
		return parsePolicy(live), err
	})
	if err != nil {
		noteFallback(err)
		return templatePolicy()
	}
	return p
}

var fallbackOnce sync.Once

// noteFallback says once per process why the template permissions apply; a missing file is a normal install.
func noteFallback(err error) {
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	fallbackOnce.Do(func() {
		cause := "settings.json unreadable or invalid (" + err.Error() + ")"
		if errors.Is(err, settings.ErrTooLarge) {
			cause = "settings.json over 4 MiB"
		}
		fmt.Fprintln(os.Stderr, "safety-scan: "+cause+": using template permissions; custom deny rows not applied")
	})
}

// parsePolicy keeps Bash rules and Edit/Write path args; an unparseable rule is skipped, as Claude Code skips it.
func parsePolicy(l settings.PermissionLists) policy {
	var p policy
	for _, set := range []struct {
		in   []string
		bash *[]permission.Rule
		edit *[]string
	}{{l.Allow, &p.bashAllow, nil}, {l.Ask, &p.bashAsk, &p.editAsk}, {l.Deny, &p.bashDeny, &p.editDeny}} {
		for _, s := range set.in {
			if r, err := permission.ParseRule(s); err == nil && r.Tool == "Bash" {
				*set.bash = append(*set.bash, r)
				continue
			}
			if set.edit != nil {
				*set.edit = append(*set.edit, editGlobs(s)...)
			}
		}
	}
	return p
}

// editGlobs is the raw path glob of an Edit/Write row (raw arg: ParseRule's ~ expansion loses the anchor).
func editGlobs(row string) []string {
	if row == "Edit" || row == "Write" { // a bare tool row covers every path
		return []string{"**/*"}
	}
	for _, tool := range []string{"Edit(", "Write("} {
		g, ok := strings.CutPrefix(row, tool)
		if !ok || !strings.HasSuffix(g, ")") {
			continue
		}
		if g = strings.TrimSpace(strings.TrimSuffix(g, ")")); g != "" {
			return []string{g}
		}
	}
	return nil
}

// settingsRuleHit is the first of rules a segment of cmd runs (wrappers, sh -c, substitutions seen through), else "".
func settingsRuleHit(cmd, cwd string, rules []permission.Rule) string {
	judge := func(rs []permission.Rule) bool {
		return permission.Permissions{Mode: permission.PermAllow, Deny: rs}.DecideAt(cwd, "Bash", cmd) == permission.Deny
	}
	if len(rules) == 0 || !judge(rules) {
		return ""
	}
	for _, r := range rules {
		if judge([]permission.Rule{r}) {
			return r.String()
		}
	}
	return "command nests too deep to check" // decideBash fails closed past depth 8
}

// settingsAllowed is Claude's allow test on Bash rules: bare Bash or Bash(*) allows all, else every segment must hit.
func settingsAllowed(cmd, cwd string, rules []permission.Rule) bool {
	rules = slices.DeleteFunc(slices.Clone(rules), func(r permission.Rule) bool { return r.Tool != "Bash" })
	if slices.ContainsFunc(rules, func(r permission.Rule) bool { return r.Arg == "" || r.Arg == "*" }) {
		return true
	}
	return len(rules) > 0 && permission.Permissions{Mode: permission.PermPlan, Allow: rules}.DecideAt(cwd, "Bash", cmd) == permission.Allow
}

// sudoDenied reports a settings deny row aimed at sudo (D2); read raw, since DecideAt unwraps sudo as a wrapper.
func sudoDenied() bool {
	return slices.ContainsFunc(livePolicy().bashDeny, func(r permission.Rule) bool {
		a := strings.TrimSpace(r.Arg)
		return r.Tool == "Bash" && (a == "sudo" || a == "sudo:*" || a == "sudo *")
	})
}

// askHit is one settings ask match: the rule tool and target the dispatch judges, and the rule its --allow names.
type askHit struct{ what, tool, arg, rule string }

// dispatchAsk decides a settings ask where the agent has no ask (D1): dispatch deny blocks, allow runs, else blocks.
func dispatchAsk(in *core.Input, h askHit) (core.Decision, string) {
	p, set, err := dispatchPolicy()
	if err != nil || !set {
		return core.Block, core.SafetyMessage(in, core.Block, h.what, h.rule, "No dispatch policy decides it."+noAskRoute+
			" Or re-dispatch with --allow '"+h.rule+"' (claude-code agents ask).")
	}
	if (permission.Permissions{Mode: permission.PermAllow, Deny: p.Deny}).DecideAt(in.CWD, h.tool, h.arg) == permission.Deny {
		return core.Block, core.SafetyMessage(in, core.Block, h.what+"; the dispatch's --deny refuses it", h.rule, ownerRoute)
	}
	if h.tool == "Bash" && settingsAllowed(h.arg, in.CWD, p.Allow) ||
		h.tool != "Bash" && (permission.Permissions{Mode: permission.PermPlan, Allow: p.Allow}).DecideAt(in.CWD, h.tool, h.arg) == permission.Allow {
		return core.Allow, ""
	}
	return core.Block, core.SafetyMessage(in, core.Block, h.what, h.rule,
		"The dispatching agent decides: re-dispatch with --allow '"+h.rule+"' (claude-code agents ask) to run it.")
}

// anchorGlob resolves a user-settings path pattern: //abs, ~/home, /config-dir, **/anywhere, else cwd ("" if none).
func anchorGlob(g, cwd string) string {
	switch {
	case strings.HasPrefix(g, "//"):
		g = g[1:]
	case strings.HasPrefix(g, "~/"):
		g = paths.ExpandHome(g)
	case strings.HasPrefix(g, "/"):
		g = filepath.Join(paths.ConfigDir(), g)
	case strings.HasPrefix(g, "**/"):
	case cwd == "":
		return ""
	default:
		g = filepath.Join(cwd, strings.TrimPrefix(g, "./"))
	}
	return paths.FoldCase(paths.CleanGlob(g))
}

// foldedGlobs are raw globs anchored at cwd and case-folded for matchesProtected; unanchorable ones drop.
func foldedGlobs(raw []string, cwd string) []string {
	var out []string
	for _, g := range raw {
		if a := anchorGlob(g, cwd); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// globNeedle is the longest wildcard-free run of an anchored glob, home-relative, that a script must mention.
func globNeedle(g, home string) string {
	var best string
	a := anchorGlob(g, "")
	if a == "" { // a cwd-relative glob has no anchor yet; its own text is the needle
		a = paths.FoldCase(paths.CleanGlob(strings.TrimPrefix(g, "./")))
	}
	for _, part := range strings.FieldsFunc(strings.TrimPrefix(a, paths.FoldCase(home)), func(r rune) bool {
		return r == '*' || r == '?'
	}) {
		if part = strings.TrimLeft(part, "/"); len(part) > len(best) {
			best = part
		}
	}
	if len(best) < 4 {
		return ""
	}
	return best
}

// extraGlobs are the built-in protected files: settings.local.json, and for a leaf settings.json and the state dir.
func extraGlobs(leaf bool) []string {
	globs := settingsGlobs(leaf)
	if leaf {
		globs = append(globs, stateGlobs()...)
	}
	return globs
}

// settingsGlobs are extraGlobs without the state dir.
func settingsGlobs(leaf bool) []string {
	globs := []string{"/" + filepath.Join(paths.ConfigDir(), "settings.local.json")}
	if leaf {
		globs = append(globs, "/"+filepath.Join(paths.ConfigDir(), "settings.json"))
	}
	return globs
}

// stateGlobs are the harness state dir as "//abs" globs, spelled as configured and symlink-resolved.
func stateGlobs() []string {
	globs := []string{"/" + filepath.Join(paths.StateDir(), "**")}
	if real := "/" + filepath.Join(paths.Real(paths.StateDir()), "**"); real != globs[0] {
		globs = append(globs, real)
	}
	return globs
}

// stateSpelling respells a path under the resolved state dir under the configured one, as grant rules spell it.
func stateSpelling(f string) string {
	if rel, ok := paths.RelWithin(paths.Real(paths.StateDir()), paths.Real(f)); ok {
		return filepath.Join(paths.StateDir(), rel)
	}
	return f
}

// leafWallVerdict blocks a leaf's dispatch-env tamper and harness-state write as floor walls (R13, 2026-11-22).
func leafWallVerdict(in *core.Input, cmd, plain string) (core.Decision, string, string) {
	if !isLeaf(in) {
		return core.Allow, "", ""
	}
	if name := dispatchTamper(in); name != "" && !bashGranted(in.CWD, cmd) {
		what := "changes " + name + "; a nested dispatch inherits its ancestors' limits and deny rules"
		return core.Block, core.SafetyMessage(in, core.Block, what, "", "Leave the HARNESS_DISPATCH_* variables as the "+
			"dispatch set them."+ownerRoute), "dispatch env tamper"
	}
	if !mayTouchState(cmd, in.CWD) {
		return core.Allow, "", ""
	}
	globs := stateGlobs()
	p := protectedWrite(cmd, in.CWD, globs, false)
	if p == "" {
		p = stateOperandWrite(cmd, in.CWD)
	}
	if p != "" && !bashGranted(in.CWD, cmd) && !wallGranted(in.CWD, p) {
		return core.Block, core.SafetyMessage(in, core.Block, "writes the harness state dir, which a leaf only reads", p,
			stateHint), "harness state write"
	}
	if needle, _ := stateInterpreterWrite(cmd, plain, globs); needle != "" && !bashGranted(in.CWD, cmd) {
		return core.Block, core.SafetyMessage(in, core.Block, "an interpreter script that may write the harness state dir",
			needle, stateHint), "interpreter harness state write"
	}
	return core.Allow, "", ""
}

// mayTouchState over-approximates reachability; the full wall still decides whether a command writes state.
func mayTouchState(cmd, cwd string) bool {
	if strings.Contains(paths.FoldCase(cmd), "state") || strings.ContainsAny(cmd, "$\\'\"`*?[{") || cwd != "" && paths.InsideDir(cwd, paths.StateDir()) {
		return true
	}
	// A relative target, a case variant or a symlink reaches the dir without naming it: resolve each target.
	inside := func(p string) bool { return paths.InsideDir(p, paths.StateDir()) }
	return slices.ContainsFunc(core.WrittenPaths(cmd, cwd), inside) || slices.ContainsFunc(core.RemovedPaths(cmd, cwd), inside) ||
		stateOperandWrite(cmd, cwd) != ""
}

// Only literal destinations extend the old interpreter wall; folded path text in output data is not a destination.
var stateLiteralDestination = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\b(?:File|IO)\.(?:binwrite|write|delete|unlink|rename)\s*\(\s*%q\([^()\\]*\)`)
})

func stateInterpreterWrite(cmd, plain string, globs []string) (string, string) {
	needle, glob := interpreterWrite(plain, globs, false)
	if needle == "" || strings.Contains(cmd, "state") || strings.ContainsAny(cmd, "$\\'\"`*?[{") {
		return needle, glob
	}
	destinations := strings.Join(stateLiteralDestination().FindAllString(plain, -1), "\n")
	return mentionedGlob(globs, paths.FoldCase(destinations), false)
}

// stateOperandWrite is the first state path a touch, ln, install -d or -t DIR copy creates ("" if none).
func stateOperandWrite(cmd, cwd string) string {
	for _, p := range core.OperandWrites(cmd, cwd) {
		if paths.InsideDir(p, paths.StateDir()) {
			return p
		}
	}
	return ""
}

const stateHint = "The harness writes its own state; a dispatch the main session starts may lift this with " +
	"--allow 'Write(/state/harness/<path>)' or a Bash rule for the command."

// mainGrant is the dispatch's Allow and Deny rules when the main session started it (record: no parent, depth 0).
func mainGrant() (permission.Permissions, bool) {
	p, set, err := dispatchPolicy()
	id := os.Getenv(proc.DispatchIDEnv)
	if err != nil || !set || len(p.Allow) == 0 || id == "" || strings.TrimSpace(os.Getenv(proc.DispatchDepthEnv)) != "0" {
		return p, false
	}
	rec, err := agent.LoadRecord(id) // the record, not the env, says who started it; a leaf cannot write it
	return p, err == nil && rec.ID == id && rec.Parent == "" && rec.Depth == 0
}

// shellForms are Bash rule args naming a shell-aware check, never an explicit grant of one command.
var shellForms = []string{permission.ReadOnlyShellArg, permission.NotReadOnlyShellArg, permission.VerifyShellArg,
	permission.NotVerifyShellArg, permission.RedirectFileArg, permission.SedPrintArg}

// scopedArg reports a rule arg naming something narrower than everything (not "", "*", "**" or "/**").
func scopedArg(arg string) bool { return strings.Trim(arg, "*/ :") != "" }

// bashGranted reports a main-started dispatch's explicit Bash allow covering cmd, with no dispatch deny on it.
func bashGranted(cwd, cmd string) bool {
	p, ok := mainGrant()
	if !ok || (permission.Permissions{Mode: permission.PermAllow, Deny: p.Deny}).DecideAt(cwd, "Bash", cmd) == permission.Deny {
		return false
	}
	rules := slices.DeleteFunc(slices.Clone(p.Allow), func(r permission.Rule) bool {
		return r.Tool != "Bash" || !scopedArg(r.Arg) || slices.Contains(shellForms, r.Arg)
	})
	return settingsAllowed(cmd, cwd, rules)
}

// editProtected reports f (typed as form) under globs; a main-granted state path is judged without the state glob.
func editProtected(cwd, f, form string, globs []string) bool {
	hit := matchesProtected(f, globs) || f != form && protectedPath(f, globs)
	if !hit || !wallGranted(cwd, f) { // dropping the state glob only removes matches
		return hit
	}
	state := foldedGlobs(stateGlobs(), cwd)
	globs = slices.DeleteFunc(slices.Clone(globs), func(g string) bool { return slices.Contains(state, g) })
	return matchesProtected(f, globs) || f != form && protectedPath(f, globs)
}

// wallGranted reports a main-started dispatch's explicit Edit/Write allow covering f under the state dir.
func wallGranted(cwd, f string) bool {
	if !protectedPath(f, foldedGlobs(stateGlobs(), cwd)) {
		return false
	}
	p, ok := mainGrant()
	if !ok {
		return false
	}
	f = stateSpelling(f)
	deny := permission.Permissions{Mode: permission.PermAllow, Deny: p.Deny}
	if deny.DecideAt(cwd, "Write", f) == permission.Deny || deny.DecideAt(cwd, "Edit", f) == permission.Deny {
		return false
	}
	return slices.ContainsFunc(p.Allow, func(r permission.Rule) bool {
		raw := []string{r.Arg}
		if strings.HasPrefix(r.Arg, "/") { // the settings spelling (/ = config dir) and the absolute one
			raw = append(raw, "/"+r.Arg)
		}
		return (r.Tool == "Edit" || r.Tool == "Write") && scopedArg(r.Arg) && protectedPath(f, foldedGlobs(raw, cwd))
	})
}
