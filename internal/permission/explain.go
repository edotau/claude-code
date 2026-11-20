package permission

import (
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/textutil"
)

// Denial identifies a blocked call and its deciding policy; Replay holds flags for an existing dispatch replay.
type Denial struct {
	Call   Rule
	Layer  string
	Rule   Rule
	Source string
	Remedy string
	Replay string
}

// Explain returns zero for an allowed call; an allow remedy is offered only when that rule repairs Decide.
func (p Permissions) Explain(tool, arg string) Denial { return p.ExplainAt("", tool, arg) }

// ExplainAt is Explain for a call run in cwd, matching DecideAt.
func (p Permissions) ExplainAt(cwd, tool, arg string) Denial {
	if p.DecideAt(cwd, tool, arg) == Allow {
		return Denial{}
	}
	d := Denial{Call: Rule{Tool: tool, Arg: arg}, Layer: "permissions"}
	for _, r := range p.Deny {
		if (Permissions{Deny: []Rule{r}}).DecideAt(cwd, tool, arg) == Deny {
			d.Rule, d.Source = r, "permissions.deny"
			d.Remedy = "remove or narrow the deciding deny rule at its source: " + r.String()
			return d
		}
	}
	for _, r := range p.Ask {
		if anyRule([]Rule{r}, tool, arg, false) {
			d.Rule, d.Source = r, "permissions.ask"
			break
		}
	}
	if d.Source == "" {
		d.Source = "permissions.mode"
	}
	d.Remedy, d.Replay = p.allowRemedy(d.Call)
	return d
}

// allowRemedy offers an exact --allow rule only when it repairs Decide; the replay equals it.
func (p Permissions) allowRemedy(call Rule) (remedy, replay string) {
	if hasRulePattern(call.Tool, call.Arg) {
		return "the call contains a wildcard or rule keyword; write the rule by hand", ""
	}
	candidate, err := ParseRule(call.String())
	if err == nil && candidate == call {
		repaired, _ := Resolve(p, Permissions{Allow: []Rule{candidate}})
		if repaired.Decide(call.Tool, call.Arg) == Allow {
			flag := "--allow " + textutil.ShellQuote(candidate.String())
			return flag, flag
		}
	}
	return "review the deciding permission mode, ask rules, or shell restrictions at their source", ""
}

// hasRulePattern reports an argument the matcher reads as a pattern or keyword; no escape exists, so no exact rule can.
func hasRulePattern(tool, arg string) bool {
	return strings.ContainsAny(arg, `*?\`) || (tool == "Bash" && (strings.HasPrefix(arg, cdPrefix) ||
		slices.Contains([]string{RedirectFileArg, SedPrintArg, ReadOnlyShellArg, NotReadOnlyShellArg}, arg))) ||
		(tool == "WebFetch" && strings.HasPrefix(arg, "domain:"))
}
