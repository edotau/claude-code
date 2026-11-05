package agent

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/shell"
)

// Decision is a permission verdict; the zero value is Deny so an unset result fails closed.
type Decision int

const (
	Deny Decision = iota
	Ask
	Allow
)

// Rule is one Claude Code style rule: Tool alone matches every call, Tool(Arg) narrows by command, path or domain.
type Rule struct{ Tool, Arg string }

// Permissions is a dispatch's rule set; Mode ("", "default", "auto", "plan") decides what no rule matched.
type Permissions struct {
	Mode  string `json:"mode,omitempty"`
	Allow []Rule `json:"allow,omitempty"`
	Ask   []Rule `json:"ask,omitempty"`
	Deny  []Rule `json:"deny,omitempty"`
}

// IsZero reports a request with no policy: no mode and no rules.
func (p Permissions) IsZero() bool { return p.Mode == "" && len(p.Allow)+len(p.Ask)+len(p.Deny) == 0 }

// String is the rule in ParseRule's syntax.
func (r Rule) String() string {
	if r.Arg == "" {
		return r.Tool
	}
	return r.Tool + "(" + r.Arg + ")"
}

// RuleStrings renders rules back to their flag form, "Tool" or "Tool(arg)".
func RuleStrings(rules []Rule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.String())
	}
	return out
}

// ParseRule parses "Tool" or "Tool(arg)".
func ParseRule(s string) (Rule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Rule{}, errors.New("permission rule is empty")
	}
	open := strings.IndexByte(s, '(')
	if open < 0 {
		if strings.ContainsRune(s, ')') {
			return Rule{}, fmt.Errorf("permission rule %q: unbalanced parentheses", s)
		}
		return Rule{Tool: s}, nil
	}
	if !strings.HasSuffix(s, ")") {
		return Rule{}, fmt.Errorf("permission rule %q: unbalanced parentheses", s)
	}
	tool, arg := strings.TrimSpace(s[:open]), s[open+1:len(s)-1]
	switch {
	case tool == "":
		return Rule{}, fmt.Errorf("permission rule %q: empty tool name", s)
	case strings.TrimSpace(arg) == "":
		return Rule{}, fmt.Errorf("permission rule %q: empty argument (write %q for every call)", s, tool)
	}
	return Rule{Tool: tool, Arg: arg}, nil
}

// ParsePermissions builds a validated Permissions from a mode and rule strings.
func ParsePermissions(mode string, allow, ask, deny []string) (Permissions, error) {
	switch mode {
	case "", "default", "auto", "plan":
	default:
		return Permissions{}, fmt.Errorf("unknown permission mode %q (want default, auto or plan)", mode)
	}
	p := Permissions{Mode: mode}
	for _, set := range []struct {
		in  []string
		out *[]Rule
	}{{allow, &p.Allow}, {ask, &p.Ask}, {deny, &p.Deny}} {
		for _, s := range set.in {
			r, err := ParseRule(s)
			if err != nil {
				return Permissions{}, err
			}
			*set.out = append(*set.out, r)
		}
	}
	return p, nil
}

// Decide applies Deny > Ask > Allow, then the mode fallback: plan denies, every other mode asks (auto: the agent's judge).
func (p Permissions) Decide(tool, arg string) Decision {
	if tool == "Bash" {
		return p.decideBash(arg, 0)
	}
	return p.decideOne(tool, arg)
}

// decideBash is the strictest verdict over every segment, substitution and `bash -c` body; redirects and substitutions never Allow.
func (p Permissions) decideBash(cmd string, depth int) Decision {
	if depth > 8 {
		return Deny
	}
	outer, subs := shell.ExtractSubstitutions(cmd)
	worst, seen := Allow, false
	for _, sub := range subs {
		worst = min(worst, p.decideBash(sub, depth+1), Ask)
	}
	for _, seg := range shell.Segments(outer) {
		toks, _ := shell.Unwrap(shell.Tokens(seg), "")
		if len(toks) == 0 {
			continue
		}
		seen = true
		d := min(p.decideOne("Bash", strings.Join(toks, " ")), p.decideOne("Bash", strings.TrimSpace(seg)))
		if nested, ok := shell.NestedCommand(toks); ok {
			d = min(d, p.decideBash(nested, depth+1), Ask)
		}
		if d == Allow && strings.ContainsAny(seg, "<>") {
			d = Ask
		}
		worst = min(worst, d)
	}
	if !seen {
		return p.decideOne("Bash", strings.TrimSpace(cmd))
	}
	return worst
}

func (p Permissions) decideOne(tool, arg string) Decision {
	switch {
	case anyRule(p.Deny, tool, arg):
		return Deny
	case anyRule(p.Ask, tool, arg):
		return Ask
	case anyRule(p.Allow, tool, arg):
		return Allow
	case p.Mode == "plan":
		return Deny
	}
	return Ask
}

func anyRule(rules []Rule, tool, arg string) bool {
	for _, r := range rules {
		if r.matches(tool, arg) {
			return true
		}
	}
	return false
}

func (r Rule) matches(tool, arg string) bool {
	if r.Tool != tool {
		return false
	}
	switch {
	case r.Arg == "":
		return true
	case tool == "Bash":
		return commandMatches(r.Arg, arg)
	case tool == "WebFetch" && strings.HasPrefix(r.Arg, "domain:"):
		return domainMatches(strings.TrimPrefix(r.Arg, "domain:"), arg)
	}
	return pathMatches(r.Arg, arg)
}

// commandMatches: "p:*" is a prefix at a word boundary, anything else must equal the command.
func commandMatches(pattern, cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	prefix, ok := strings.CutSuffix(pattern, ":*")
	if !ok {
		return wildcardMatches(pattern, cmd)
	}
	rest, has := strings.CutPrefix(cmd, prefix)
	return has && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// wildcardMatches is Claude Code's Bash wildcard: "*" spans any text and a trailing " *" also matches the bare prefix.
func wildcardMatches(pattern, cmd string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == cmd
	}
	if prefix, ok := strings.CutSuffix(pattern, " *"); ok && cmd == prefix {
		return true
	}
	parts := strings.Split(pattern, "*")
	rest, ok := strings.CutPrefix(cmd, parts[0])
	if !ok {
		return false
	}
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(rest, mid)
		if i < 0 {
			return false
		}
		rest = rest[i+len(mid):]
	}
	return strings.HasSuffix(rest, parts[len(parts)-1])
}

// domainMatches takes a bare host or a URL and accepts the domain itself and its subdomains.
func domainMatches(domain, target string) bool {
	host := target
	if u, err := url.Parse(target); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	host = strings.ToLower(host)
	domain = strings.ToLower(domain)
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// pathMatches globs a path: "*" and "?" stay inside a segment, "**" crosses directories, a leading "./" is ignored.
func pathMatches(pattern, p string) bool {
	re, err := regexp.Compile("^" + globRegexp(cleanGlobPath(pattern)) + "$")
	return err == nil && re.MatchString(cleanGlobPath(p))
}

func cleanGlobPath(s string) string {
	s = strings.ReplaceAll(s, "**", "\x00")
	s = path.Clean(strings.ReplaceAll(s, "\\", "/"))
	return strings.ReplaceAll(s, "\x00", "**")
}

func globRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		switch c := g[i]; {
		case c == '*' && strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case c == '*' && strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}
