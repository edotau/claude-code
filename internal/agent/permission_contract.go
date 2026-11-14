package agent

import (
	"errors"
	"strings"

	"github.com/edotau/claude-code/internal/agent/retry"
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
func (p Permissions) Explain(tool, arg string) Denial {
	if p.Decide(tool, arg) == Allow {
		return Denial{}
	}
	d := Denial{Call: Rule{Tool: tool, Arg: arg}, Layer: "permissions"}
	for _, r := range p.Deny {
		if (Permissions{Deny: []Rule{r}}).Decide(tool, arg) == Deny {
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
		repaired, _ := resolvePermissions(p, Permissions{Allow: []Rule{candidate}})
		if repaired.Decide(call.Tool, call.Arg) == Allow {
			flag := "--allow " + textutil.ShellQuote(candidate.String())
			return flag, flag
		}
	}
	return "review the deciding permission mode, ask rules, or shell restrictions at their source", ""
}

// hasRulePattern reports an argument the matcher reads as a pattern or keyword; no escape exists, so no exact rule can.
func hasRulePattern(tool, arg string) bool {
	switch {
	case strings.ContainsAny(arg, `*?\`):
		return true
	case tool == "Bash":
		switch arg {
		case RedirectFileArg, SedPrintArg, ReadOnlyShellArg, NotReadOnlyShellArg:
			return true
		}
		return strings.HasPrefix(arg, cdPrefix)
	}
	return tool == "WebFetch" && strings.HasPrefix(arg, "domain:")
}

// CheckCapabilities refuses an unenforceable --no-tools or permission feature; other unsupported flags run, dropped.
func CheckCapabilities(name string, req RunOptions) error {
	r, err := Lookup(name)
	if err == nil {
		err = capabilityRefusal(r.Caps(), name, req)
	}
	if err != nil {
		return retry.Fatal(name, err)
	}
	return nil
}

// capabilityRefusal is the first capability c cannot honor for req, or nil.
func capabilityRefusal(c Caps, name string, req RunOptions) error {
	if err := refusePersona(c, name, req.Persona); err != nil {
		return err
	}
	if err := refusePermissions(c, name, req.Permissions); err != nil {
		return err
	}
	switch {
	case req.NoTools && c.NoTools == None:
		return errors.New("unsupported capability: --no-tools")
	case req.Tools && req.NoTools:
		return errors.New("incompatible capabilities: --tools and --no-tools")
	case req.ReadOnly && c.ReadOnly == Emulated && c.Permissions == None:
		return errors.New("--read-only requires permission enforcement")
	}
	return nil
}
