package browser

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const robotsAgent = "claude-code"

type robotsRule struct {
	allow   bool
	pattern string
}

// robotsGate fetches and caches robots.txt per origin and judges URLs against it.
type robotsGate struct {
	ignore bool
	client *http.Client
	rules  map[string][]robotsRule
}

func newRobotsGate(ignore bool) *robotsGate {
	return &robotsGate{ignore: ignore, client: &http.Client{Timeout: 10 * time.Second}, rules: map[string][]robotsRule{}}
}

// check refuses a URL its origin's robots.txt disallows for claude-code; 404 or unreachable means allowed.
func (g *robotsGate) check(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("only http and https URLs are supported: %q", raw)
	}
	if g.ignore {
		return nil
	}
	origin := u.Scheme + "://" + u.Host
	rules, ok := g.rules[origin]
	if !ok {
		rules = g.fetch(ctx, origin)
		g.rules[origin] = rules
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	if r, blocked := robotsDisallows(rules, path); blocked {
		return fmt.Errorf("robots.txt at %s disallows %s (rule \"Disallow: %s\"); --ignore-robots overrides", origin, path, r.pattern)
	}
	return nil
}

func (g *robotsGate) fetch(ctx context.Context, origin string) []robotsRule {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/robots.txt", nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", robotsAgent)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	return parseRobots(string(body))
}

// parseRobots returns the rules of the claude-code group, else the * group.
func parseRobots(txt string) []robotsRule {
	groups := map[string][]robotsRule{}
	var agents []string
	inRules := false
	for line := range strings.SplitSeq(txt, "\n") {
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
			if v == "" {
				continue
			}
			for _, a := range agents {
				groups[a] = append(groups[a], robotsRule{allow: k == "allow", pattern: v})
			}
		}
	}
	if r, ok := groups[robotsAgent]; ok {
		return r
	}
	return groups["*"]
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

// robotsMatch matches a prefix pattern with * wildcards and an optional trailing $ anchor.
func robotsMatch(pattern, path string) bool {
	if pattern == "" {
		return true
	}
	if pattern == "$" {
		return path == ""
	}
	if pattern[0] == '*' {
		for i := 0; i <= len(path); i++ {
			if robotsMatch(pattern[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	return path != "" && path[0] == pattern[0] && robotsMatch(pattern[1:], path[1:])
}
