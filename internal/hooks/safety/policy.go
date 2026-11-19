package safety

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
)

// policy is the settings.json permissions safety-scan reads: Bash rules per tier, Edit/Write globs as written.
type policy struct {
	bashAllow, bashAsk, bashDeny []agent.Rule
	editAsk, editDeny            []string
}

var (
	policyCache    paths.StampCache[policy]
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
		return templatePolicy()
	}
	return p
}

// parsePolicy keeps Bash rules and Edit/Write path args; an unparseable rule is skipped, as Claude Code skips it.
func parsePolicy(l settings.PermissionLists) policy {
	var p policy
	for _, set := range []struct {
		in   []string
		bash *[]agent.Rule
		edit *[]string
	}{{l.Allow, &p.bashAllow, nil}, {l.Ask, &p.bashAsk, &p.editAsk}, {l.Deny, &p.bashDeny, &p.editDeny}} {
		for _, s := range set.in {
			if r, err := agent.ParseRule(s); err == nil && r.Tool == "Bash" {
				*set.bash = append(*set.bash, r)
				continue
			}
			for _, tool := range []string{"Edit(", "Write("} { // raw arg: ParseRule's ~ expansion loses the anchor
				if g, ok := strings.CutPrefix(s, tool); ok && set.edit != nil && strings.HasSuffix(g, ")") && len(g) > 1 {
					*set.edit = append(*set.edit, strings.TrimSuffix(g, ")"))
				}
			}
		}
	}
	return p
}

// settingsRuleHit is the first of rules a segment of cmd runs (wrappers, sh -c, substitutions seen through), else "".
func settingsRuleHit(cmd, cwd string, rules []agent.Rule) string {
	judge := func(rs []agent.Rule) bool {
		return agent.Permissions{Mode: agent.PermAllow, Deny: rs}.DecideAt(cwd, "Bash", cmd) == agent.Deny
	}
	if len(rules) == 0 || !judge(rules) {
		return ""
	}
	for _, r := range rules {
		if judge([]agent.Rule{r}) {
			return r.String()
		}
	}
	return "command nests too deep to check" // decideBash fails closed past depth 8
}

// settingsAllowed is Claude's allow test on Bash rules: bare Bash or Bash(*) allows all, else every segment must hit.
func settingsAllowed(cmd, cwd string, rules []agent.Rule) bool {
	rules = slices.DeleteFunc(slices.Clone(rules), func(r agent.Rule) bool { return r.Tool != "Bash" })
	if slices.ContainsFunc(rules, func(r agent.Rule) bool { return r.Arg == "" || r.Arg == "*" }) {
		return true
	}
	return len(rules) > 0 && agent.Permissions{Mode: agent.PermPlan, Allow: rules}.DecideAt(cwd, "Bash", cmd) == agent.Allow
}

// sudoDenied reports a settings deny rule `sudo true` meets (D2); without one the built-in sudo guard is off.
func sudoDenied() bool { return settingsRuleHit("sudo true", "", livePolicy().bashDeny) != "" }

// askHit is one settings ask match: the rule tool and target the dispatch judges, and the rule its --allow names.
type askHit struct{ what, tool, arg, rule string }

// dispatchAsk decides a settings ask where the agent has no ask (D1): dispatch deny blocks, allow runs, else blocks.
func dispatchAsk(in *core.Input, h askHit) (core.Decision, string) {
	p, set, err := dispatchPolicy()
	if err != nil || !set {
		return core.Block, core.SafetyMessage(in, core.Block, h.what, h.rule, "No dispatch policy decides it."+noAskRoute+
			" Or re-dispatch with --allow '"+h.rule+"' (claude-code agents ask).")
	}
	if (agent.Permissions{Mode: agent.PermAllow, Deny: p.Deny}).DecideAt(in.CWD, h.tool, h.arg) == agent.Deny {
		return core.Block, core.SafetyMessage(in, core.Block, h.what+"; the dispatch's --deny refuses it", h.rule, ownerRoute)
	}
	if h.tool == "Bash" && settingsAllowed(h.arg, in.CWD, p.Allow) ||
		h.tool != "Bash" && (agent.Permissions{Mode: agent.PermPlan, Allow: p.Allow}).DecideAt(in.CWD, h.tool, h.arg) == agent.Allow {
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
	for _, part := range strings.FieldsFunc(strings.TrimPrefix(anchorGlob(g, ""), paths.FoldCase(home)), func(r rune) bool {
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

// extraGlobs are the built-in protected files: settings.local.json, and settings.json for a leaf ("//abs" anchors).
func extraGlobs(leaf bool) []string {
	globs := []string{"/" + filepath.Join(paths.ConfigDir(), "settings.local.json")}
	if leaf {
		globs = append(globs, "/"+filepath.Join(paths.ConfigDir(), "settings.json"))
	}
	return globs
}
