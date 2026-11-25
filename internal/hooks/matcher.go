package hooks

import (
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/wthook"
	"github.com/edotau/claude-code/internal/proc"
)

// compiled is a handler matcher: literal alternatives, or an anchored case-insensitive regexp; both nil never match.
type compiled struct {
	alts []string
	re   *regexp.Regexp
}

var matcherCache sync.Map // matcher string -> *compiled, built once per process

func compileMatcher(matcher string) *compiled {
	if c, ok := matcherCache.Load(matcher); ok {
		return c.(*compiled)
	}
	c := &compiled{}
	if strings.ContainsAny(matcher, `\.+*?()[]{}^$`) {
		c.re, _ = regexp.Compile(`(?i)^(?:` + matcher + `)$`)
	} else {
		c.alts = strings.Split(matcher, "|")
	}
	act, _ := matcherCache.LoadOrStore(matcher, c)
	return act.(*compiled)
}

// matchTool applies a handler matcher to a tool name; empty matches everything, anchored and case-insensitive.
func matchTool(matcher, tool string) bool {
	if matcher == "" {
		return true
	}
	if tool == "" {
		return false
	}
	c := compileMatcher(matcher)
	if c.re != nil {
		return c.re.MatchString(tool)
	}
	for _, a := range c.alts {
		if strings.EqualFold(a, tool) {
			return true
		}
	}
	return false
}

// placementRun and claimCheckRun keep the leaf lift: a leaf's Bash block or ask from the module is allowed with a note.
func placementRun(in *core.Input) core.Result  { return leafBashAllow(docs.Placement, in) }
func claimCheckRun(in *core.Input) core.Result { return leafBashAllow(wthook.ClaimCheck, in) }

// leafBashAllow is run(in), except a leaf's Bash block or ask (not a Floor block) becomes allow plus a note.
func leafBashAllow(run core.Runner, in *core.Input) core.Result {
	r := run(in)
	if r.Decision == core.Allow || r.Floor || !strings.EqualFold(in.CanonicalTool(), "bash") {
		return r
	}
	if in.AgentID == "" && os.Getenv(proc.DispatchIDEnv) == "" {
		return r
	}
	note := strings.TrimSpace(r.Reason)
	if r.Rule != "" && note != "" {
		note = r.Rule + "\n" + note
	} else {
		note = r.Rule + note
	}
	r.Context = core.JoinContext(r.Context, "allowed for an agent (owner order): "+note)
	r.Decision, r.Reason, r.Verdict = core.Allow, "", core.VerdictPassed
	return r
}
