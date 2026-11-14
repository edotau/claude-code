package agent

import (
	"errors"
	"fmt"
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

// CheckCapabilities refuses unsupported requests without changing the request or existing launch behavior.
func CheckCapabilities(name string, req RunOptions) error {
	r, err := Lookup(name)
	if err != nil {
		return retry.Fatal(name, err)
	}
	c := r.Caps()
	if err := refusePersona(c, name, req.Persona); err != nil {
		return retry.Fatal(name, err)
	}
	if err := refusePermissions(c, name, req.Permissions); err != nil {
		return retry.Fatal(name, err)
	}
	_, dropped, _ := applyCaps(c, req)
	if len(dropped) > 0 {
		return retry.Fatal(name, fmt.Errorf("unsupported capability: %s", strings.Join(dropped, ", ")))
	}
	if req.Tools && req.NoTools {
		return retry.Fatal(name, errors.New("incompatible capabilities: --tools and --no-tools"))
	}
	if req.ReadOnly && c.ReadOnly == Emulated && c.Permissions == None {
		return retry.Fatal(name, errors.New("--read-only requires permission enforcement"))
	}
	return nil
}
