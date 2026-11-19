package hooks

import (
	"regexp"
	"strings"
	"sync"
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
