package websearch

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/network/httpx"
)

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

// robotsMax caps the per-origin robots.txt cache the long-lived MCP server keeps; robotsTTL bounds its age.
const robotsMax = 256

var robotsTTL = 20 * time.Minute

type robotsEntry struct {
	rules []robotsRule
	at    time.Time
}

// robotsCache holds each origin's parsed robots.txt rules across requests; a fetch error is never cached.
var robotsCache = struct {
	sync.Mutex
	m map[string]robotsEntry
}{m: map[string]robotsEntry{}}

// robotsAllowed reports whether robots.txt permits u; missing allows, unreadable (5xx/transport) refuses and joins dead.
func robotsAllowed(ctx context.Context, u *url.URL, dead map[string]bool) bool {
	origin := u.Scheme + "://" + u.Host
	if dead[origin] {
		return false
	}
	rules, ok := cachedRobotsRules(ctx, origin)
	if !ok && dead != nil {
		dead[origin] = true
	}
	_, blocked := robotsDisallows(rules, u.RequestURI())
	return ok && !blocked
}

// cachedRobotsRules is origin's parsed robots.txt rules from robotsCache within robotsTTL, else fetched, parsed and cached.
func cachedRobotsRules(ctx context.Context, origin string) ([]robotsRule, bool) {
	robotsCache.Lock()
	e, ok := robotsCache.m[origin]
	robotsCache.Unlock()
	if ok && time.Since(e.at) < robotsTTL {
		return e.rules, true
	}
	body, ok := fetchRobots(ctx, origin)
	if !ok {
		return nil, false
	}
	rules := parseRobots(body)
	robotsCache.Lock()
	defer robotsCache.Unlock()
	if len(robotsCache.m) >= robotsMax {
		for k, old := range robotsCache.m {
			if time.Since(old.at) >= robotsTTL || len(robotsCache.m) >= robotsMax {
				delete(robotsCache.m, k)
			}
		}
	}
	robotsCache.m[origin] = robotsEntry{rules: rules, at: time.Now()}
	return rules, true
}

// fetchRobots returns the origin's robots.txt body ("" when missing); ok is false when it could not be read.
func fetchRobots(ctx context.Context, origin string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, robotsTimeout)
	defer cancel()
	status, raw, _, err := httpx.Do(ctx, httpx.Call{Client: pageClient, URL: origin + "/robots.txt", Limit: 512 << 10,
		Header: http.Header{"User-Agent": {userAgent}}})
	if status == 0 {
		return "", false
	}
	if status != http.StatusOK {
		return "", status < 500
	}
	if err != nil && !errors.Is(err, httpx.ErrTooLarge) {
		return "", false
	}
	return string(raw), true
}
