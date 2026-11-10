package websearch

import "strings"

// robotsAgent is the user-agent group robots.txt rules are read for.
const robotsAgent = "claude-code"

type robotsRule struct {
	allow   bool
	pattern string
}

// parseRobots returns the rules of the claude-code group, else the * group.
func parseRobots(txt string) []robotsRule {
	groups := map[string][]robotsRule{}
	var agents []string
	inRules := false
	for line := range strings.SplitSeq(strings.TrimPrefix(txt, "\ufeff"), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if inRules {
				agents, inRules = nil, false
			}
			agents = append(agents, strings.ToLower(v))
		case "allow", "disallow":
			inRules = true
			addRobotsRule(groups, agents, k == "allow", v)
		}
	}
	if r, ok := groups[robotsAgent]; ok {
		return r
	}
	return groups["*"]
}

// addRobotsRule records one rule for every agent of the group; an empty pattern only declares the group.
func addRobotsRule(groups map[string][]robotsRule, agents []string, allow bool, pattern string) {
	for _, a := range agents {
		if pattern == "" {
			if _, ok := groups[a]; !ok {
				groups[a] = nil
			}
			continue
		}
		groups[a] = append(groups[a], robotsRule{allow: allow, pattern: pattern})
	}
}

// robotsDisallows applies longest-match semantics (Allow wins ties) and returns the deciding Disallow rule.
func robotsDisallows(rules []robotsRule, path string) (robotsRule, bool) {
	var best robotsRule
	found := false
	for _, r := range rules {
		if !robotsMatch(r.pattern, path) {
			continue
		}
		if !found || len(r.pattern) > len(best.pattern) || (len(r.pattern) == len(best.pattern) && r.allow) {
			best, found = r, true
		}
	}
	return best, found && !best.allow
}

// robotsMatch matches a prefix pattern with * wildcards and an optional trailing $ anchor, in linear time.
func robotsMatch(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	pattern = strings.TrimSuffix(pattern, "$")
	segs := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, segs[0]) {
		return false
	}
	if len(segs) == 1 {
		return !anchored || path == segs[0]
	}
	path = path[len(segs[0]):]
	last := segs[len(segs)-1]
	for _, seg := range segs[1 : len(segs)-1] {
		i := strings.Index(path, seg)
		if i < 0 {
			return false
		}
		path = path[i+len(seg):]
	}
	if anchored {
		return strings.HasSuffix(path, last)
	}
	return strings.Contains(path, last)
}
