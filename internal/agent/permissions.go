package agent

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
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

// Each visits rules in strictness order: Deny, Ask, then Allow.
func (p Permissions) Each(fn func(decision Decision, r Rule)) {
	for decision, rules := range [][]Rule{p.Deny, p.Ask, p.Allow} {
		for _, r := range rules {
			fn(Decision(decision), r)
		}
	}
}

// IsZero reports a request with no policy: no mode and no rules.
func (p Permissions) IsZero() bool { return p.Mode == "" && len(p.Allow)+len(p.Ask)+len(p.Deny) == 0 }

// SafeModeDeny is the first Deny rule unenforceable flags under --safe-mode; ok is false outside safe mode.
func SafeModeDeny(req RunOptions, unenforceable func(Rule) bool) (Rule, bool) {
	if !req.SafeMode {
		return Rule{}, false
	}
	for _, r := range req.Permissions.Deny {
		if unenforceable(r) {
			return r, true
		}
	}
	return Rule{}, false
}

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
	return Rule{Tool: tool, Arg: expandHome(tool, arg)}, nil
}

// expandHome turns a leading ~/ in a path rule into the home directory; Bash and WebFetch rules are not paths.
func expandHome(tool, arg string) string {
	rest, ok := strings.CutPrefix(arg, "~/")
	if !ok || tool == "Bash" || tool == "WebFetch" {
		return arg
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return arg
	}
	return filepath.Join(home, rest)
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

// DefaultPermissions is name's default dispatch policy, the first layer resolvePermissions applies.
func DefaultPermissions(name string) (Permissions, error) {
	r, err := agentconfig.Settings(name)
	if err == nil {
		var p Permissions
		if p, err = policyPermissions(r.Permissions); err == nil {
			return p, nil
		}
	}
	return Permissions{}, fmt.Errorf("%s default permissions: %w", name, err)
}

// withReadOnlyRules adds --read-only's deny rules and shell allow where the agent enforces them itself (Emulated).
func withReadOnlyRules(c Caps, p Permissions) Permissions {
	if c.ReadOnly != Emulated {
		return p
	}
	p.Deny = append(slices.Clone(p.Deny), ReadOnlyDeny...)
	p.Allow = append(slices.Clone(p.Allow), ReadOnlyAllow) // Bash only where ReadOnlyShell approves
	return p
}

// LaunchPermissions is name's default policy as checkRun builds it, minus the outer dispatch's inherited layer.
func LaunchPermissions(name string, readOnly bool) (Permissions, error) {
	p, err := DefaultPermissions(name)
	if err != nil || !readOnly {
		return p, err
	}
	e, err := Lookup(name)
	if err != nil {
		return Permissions{}, err
	}
	return withReadOnlyRules(e.Caps(), p), nil
}

// resolvePermissions layers defaults, role, request: rules add, last mode wins, widened names what a layer grants (D3).
func resolvePermissions(layers ...Permissions) (out Permissions, widened []string) {
	for i, l := range layers {
		if i > 0 { // the first layer is the baseline the others widen
			widened = append(widened, widening(out, l)...)
		}
		ask := slices.DeleteFunc(slices.Clone(out.Ask), func(r Rule) bool { return slices.Contains(l.Allow, r) })
		out = Permissions{Mode: cmp.Or(l.Mode, out.Mode), Allow: union(out.Allow, l.Allow), Ask: union(ask, l.Ask),
			Deny: union(out.Deny, l.Deny)}
	}
	return out, widened
}

// widening is what l grants beyond base: an allow neither layer denies, or a mode that leaves plan.
func widening(base, l Permissions) []string {
	var out []string
	for _, r := range l.Allow {
		if !slices.Contains(base.Allow, r) && !slices.Contains(base.Deny, r) && !slices.Contains(l.Deny, r) {
			out = append(out, "allow "+r.String())
		}
	}
	if base.Mode == "plan" && l.Mode != "" && l.Mode != "plan" {
		out = append(out, "mode "+l.Mode)
	}
	return out
}

// union is a followed by b's rules not already in a.
func union(a, b []Rule) []Rule {
	out := slices.Clone(a)
	for _, r := range b {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// Decide applies Deny > Ask > Allow, then the mode fallback: plan denies, other modes ask (auto: the agent's judge).
func (p Permissions) Decide(tool, arg string) Decision {
	if tool == "Bash" {
		return p.decideBash(arg, 0)
	}
	return p.decideOne(tool, arg)
}

// decideBash is the strictest verdict over every segment, substitution and `bash -c` body; redirects never Allow.
func (p Permissions) decideBash(cmd string, depth int) Decision {
	if depth > 8 {
		return Deny
	}
	outer, subs := shell.ExtractSubstitutions(cmd)
	if len(subs) > 0 && slices.Contains(p.Deny, ReadOnlyDeny[0]) { // read-only never runs a substitution
		return Deny
	}
	worst, seen := Allow, false
	for _, sub := range subs {
		worst = min(worst, p.decideBash(sub, depth+1), Ask)
	}
	for _, seg := range shell.Segments(outer) {
		d, ok := p.decideSeg(seg, depth)
		seen = seen || ok
		if ok {
			worst = min(worst, d)
		}
	}
	if !seen { // a bare `$(x)` or backtick command: its substitutions' verdict still counts
		return min(worst, p.decideOne("Bash", strings.TrimSpace(cmd)))
	}
	return worst
}

// decideSeg is one segment's verdict with its nested command; heredocs, here-strings and file redirects never Allow.
func (p Permissions) decideSeg(seg string, depth int) (Decision, bool) {
	rawToks := shell.Tokens(seg)
	toks, _ := shell.Unwrap(rawToks, "")
	if len(toks) == 0 {
		return Allow, false
	}
	d := p.decideSegment(strings.TrimSpace(seg), rawToks, toks)
	if nested, ok := shell.NestedCommand(toks); ok {
		d = min(d, p.decideBash(nested, depth+1), Ask)
	}
	if d == Allow && (strings.Contains(seg, "<") || shell.FileRedirect(seg)) {
		d = Ask
	}
	if d == Allow && readOnlyReadDenied(p, seg) {
		d = Deny
	}
	return d, true
}

// RedirectFileArg is the Bash deny argument for a file redirect; it reads quotes, so only a raw spelling matches.
const RedirectFileArg = "output redirect to a file"

// decideSegment judges one segment: min of its raw and normalized spellings, unless a bare env/time prefix is stripped.
func (p Permissions) decideSegment(raw string, rawToks, toks []string) Decision {
	norm := strings.Join(toks, " ")
	if !grantsNormalized(rawToks, toks) {
		return min(p.decideForm("Bash", raw, false), p.decideForm("Bash", norm, true))
	}
	hit := func(rules []Rule) bool {
		return anyRule(rules, "Bash", raw, false) || anyRule(rules, "Bash", norm, true)
	}
	switch {
	case hit(p.Deny):
		return Deny
	case hit(p.Ask):
		return Ask
	case hit(p.Allow):
		return Allow
	case p.Mode == "plan":
		return Deny
	}
	return Ask
}

// SedPrintArg is the Bash allow argument for read-only `sed -n <range>p FILE...`; any other script or flag is no match.
const SedPrintArg = "sed -n N,Mp FILE"

var sedRange = regexp.MustCompile(`^(\d+|\$)(,(\d+|\$))?p$`)

// sedPrintMatches is `sed -n <range>p` with plain file operands: no -i, -e, -f, w/e/r commands or other option.
func sedPrintMatches(cmd string) bool {
	toks := shell.Tokens(cmd)
	if len(toks) < 4 || toks[0] != "sed" || toks[1] != "-n" || !sedRange.MatchString(toks[2]) {
		return false
	}
	return !slices.ContainsFunc(toks[3:], func(t string) bool { return strings.HasPrefix(t, "-") || strings.ContainsAny(t, "$`") })
}

const cdPrefix = "cd:"

// CdRule allows a segment that is exactly `cd <absolute path>` naming dir or a directory under it.
func CdRule(dir string) Rule { return Rule{Tool: "Bash", Arg: cdPrefix + dir} }

// cdMatches is a bare `cd <abs>` whose cleaned, expansion-free target is dir or below it; anything else is no match.
func cdMatches(dir, cmd string) bool {
	toks := shell.Tokens(cmd)
	if len(toks) != 2 || toks[0] != "cd" || !path.IsAbs(toks[1]) || strings.ContainsAny(toks[1], "$`*?[{~\\") {
		return false
	}
	target, dir := path.Clean(toks[1]), path.Clean(dir)
	return target == dir || strings.HasPrefix(target, strings.TrimSuffix(dir, "/")+"/")
}

var envAssign = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=`)

// grantsNormalized reports that the stripped prefix is only `[env|time] NAME=value…` with harmless names.
func grantsNormalized(rawToks, toks []string) bool {
	n := len(rawToks) - len(toks)
	if n <= 0 || !slices.Equal(rawToks[n:], toks) {
		return false
	}
	prefix := rawToks[:n]
	if prefix[0] == "env" || prefix[0] == "time" {
		prefix = prefix[1:]
	}
	for _, t := range prefix {
		m := envAssign.FindStringSubmatch(t)
		if m == nil || unsafeEnvName(m[1]) {
			return false
		}
	}
	return true
}

// unsafeEnvName is a name the overlay env layer refuses, or one that changes which binary or flags a command runs.
func unsafeEnvName(name string) bool {
	up := strings.ToUpper(name)
	switch up {
	case "PATH", "IFS", "BASH_ENV", "ENV", "SHELLOPTS", "BASHOPTS", "PS4", "PROMPT_COMMAND", "GOFLAGS", "GOROOT",
		"GOTOOLCHAIN", "GOENV", "GOPROXY", "GOCACHEPROG", "GOEXPERIMENT":
		return true
	}
	return strings.HasPrefix(up, "LD_") || strings.HasPrefix(up, "DYLD_") || agentconfig.SensitiveEnvName(name)
}

func (p Permissions) decideOne(tool, arg string) Decision { return p.decideForm(tool, arg, false) }

// decideForm is decideOne for a Bash command spelled raw or normalized (quotes stripped, wrappers dropped).
func (p Permissions) decideForm(tool, arg string, norm bool) Decision {
	switch {
	case anyRule(p.Deny, tool, arg, norm):
		return Deny
	case anyRule(p.Ask, tool, arg, norm):
		return Ask
	case anyRule(p.Allow, tool, arg, norm):
		return Allow
	case p.Mode == "plan":
		return Deny
	}
	return Ask
}

// anyRule reports a matching rule; the file-redirect rule reads quotes, so it skips a normalized spelling.
func anyRule(rules []Rule, tool, arg string, norm bool) bool {
	for _, r := range rules {
		if hit, ok := readOnlyRuleHit(r, tool, arg, norm); ok {
			if hit {
				return true
			}
		} else if r.matches(tool, arg) && !skipsNormalized(r, norm) {
			return true
		}
	}
	return false
}

// skipsNormalized reports a file-redirect rule meeting a normalized spelling, which has no quotes to read it from.
func skipsNormalized(r Rule, norm bool) bool {
	return norm && r.Arg == RedirectFileArg
}

// readOnlyRuleHit judges the read-only shell rules on a normalized spelling, whose quotes (and so redirects) are gone.
func readOnlyRuleHit(r Rule, tool, arg string, norm bool) (hit, handled bool) {
	if !norm || tool != "Bash" || r.Tool != tool || r.Arg != ReadOnlyShellArg && r.Arg != NotReadOnlyShellArg {
		return false, false
	}
	return r.Arg == ReadOnlyShellArg && readOnlySegment(arg, false), true
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
	switch { // the shell-aware rule forms first

	case pattern == RedirectFileArg:
		return shell.FileRedirect(cmd)
	case pattern == SedPrintArg:
		return sedPrintMatches(cmd)
	case pattern == ReadOnlyShellArg:
		return readOnlySegment(cmd, true)
	case pattern == NotReadOnlyShellArg:
		return !readOnlySegment(cmd, true)
	case strings.HasPrefix(pattern, cdPrefix):
		return cdMatches(strings.TrimPrefix(pattern, cdPrefix), cmd)
	}
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
	if !strings.Contains(target, "://") {
		target = "//" + target // a scheme-less target keeps its path out of the host
	}
	if u, err := url.Parse(target); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".") // an FQDN's trailing dot names the same host
	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	return host == domain || strings.HasSuffix(host, "."+domain)
}

var pathPatterns sync.Map

// pathMatches globs a path as written, then as canonical forms when both are absolute (/tmp equals /private/tmp).
func pathMatches(pattern, p string) bool {
	pattern, p = cleanGlobPath(pattern), cleanGlobPath(p)
	if globMatches(pattern, p) {
		return true
	}
	return path.IsAbs(pattern) && path.IsAbs(p) && globMatches(paths.CanonDeep(pattern), paths.CanonDeep(p))
}

// globMatches globs a cleaned path: "*" and "?" stay inside a segment, "**" crosses directories.
func globMatches(pattern, p string) bool {
	cached, ok := pathPatterns.Load(pattern)
	if !ok {
		re, _ := regexp.Compile("^" + globRegexp(pattern) + "$") // Cache failed compilations as non-matches.
		cached, _ = pathPatterns.LoadOrStore(pattern, re)
	}
	re := cached.(*regexp.Regexp)
	return re != nil && re.MatchString(p)
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
		default: // copy one whole rune: string(byte) would turn UTF-8 bytes into Latin-1 characters
			_, size := utf8.DecodeRuneInString(g[i:])
			b.WriteString(regexp.QuoteMeta(g[i : i+size]))
			i += size - 1
		}
	}
	return b.String()
}

// ReadOnlyDeny is --read-only as deny rules for an agent whose CLI has no read-only mode (ReadOnly: Emulated).
var ReadOnlyDeny = []Rule{{Tool: "Bash", Arg: NotReadOnlyShellArg}, {Tool: "Write"}, {Tool: "Edit"}}

// refusePermissions fails closed when req carries Deny rules (or plan mode) and the agent cannot read Permissions.
func refusePermissions(c Caps, agentName string, p Permissions) error {
	if c.Permissions != None || (len(p.Deny) == 0 && p.Mode != "plan") {
		return nil
	}
	what := "permission deny rules"
	if len(p.Deny) == 0 {
		what = "--mode plan"
	}
	return errors.New(agentName + " cannot enforce " + what + " yet; dispatch refused")
}
