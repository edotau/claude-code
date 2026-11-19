package agent

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
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

// Permissions is a dispatch's rule set; Mode ("", allow, default, auto, plan) decides what no rule matched.
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
	if strings.HasPrefix(arg, "~/") {
		arg = expandHome(tool, arg)
	}
	return Rule{Tool: tool, Arg: arg}, nil
}

// expandHome resolves home prefixes in path rules, retaining unknown users and failed lookups literally.
func expandHome(tool, arg string) string {
	if !strings.HasPrefix(arg, "~") || tool == "Bash" || tool == "WebFetch" {
		return arg
	}
	name, rest, _ := strings.Cut(arg[1:], "/")
	var home string
	if name == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return arg
		}
	} else {
		u, err := user.Lookup(name)
		if err != nil {
			return arg
		}
		home = u.HomeDir
	}
	if home == "" {
		return arg
	}
	return filepath.Join(home, rest)
}

// Permission modes for --permissions: allow is the development override (every call runs unless a rule denies it).
const (
	PermAllow   = "allow"
	PermDefault = "default"
	PermAuto    = "auto"
	PermPlan    = "plan"
)

// ParsePermissions builds a validated Permissions from a mode and rule strings.
func ParsePermissions(mode string, allow, ask, deny []string) (Permissions, error) {
	switch mode {
	case "", PermAllow, PermDefault, PermAuto, PermPlan:
	default:
		return Permissions{}, fmt.Errorf("unknown permission mode %q (want allow, default, auto or plan)", mode)
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
func (p Permissions) Decide(tool, arg string) Decision { return p.DecideAt("", tool, arg) }

// DecideAt is Decide for a command run in cwd: a static relative cd is also judged as its absolute target.
func (p Permissions) DecideAt(cwd, tool, arg string) Decision {
	if tool != "Bash" {
		return p.decideOne(tool, arg)
	}
	return p.decideBash(shellCode(arg), 0, cwd) // judge what bash runs: a comment can hide a quote that ends one
}

// shellCode is cmd without the text bash never runs as commands: comments and heredoc bodies fed to a data consumer.
func shellCode(cmd string) string {
	s := codeScan{cmd: cmd, segs: []int{0}}
	if !s.scan() {
		return cmd
	}
	if len(s.docs) == 0 {
		return without(cmd, s.cuts)
	}
	cuts := slices.Clone(s.cuts)
	if all := append(slices.Clone(s.cuts), spansOf(s.docs, false)...); !runsBody(without(cmd, all)) {
		cuts = append(cuts, spansOf(s.docs, true)...)
	}
	return without(cmd, cuts)
}

type span struct{ from, to int }

// heredoc is a `<<DELIM` operator: span is its body plus closing line; data means a non-shell consumer reads it.
type heredoc struct {
	span
	delim              string
	dash, quoted, data bool
}

// codeScan tracks quoting through cmd; stack holds open `(` code, `"` and `{` (${…}) contexts.
type codeScan struct {
	cmd     string
	stack   []byte
	segs    []int // start of the current segment, one per code context
	pending []heredoc
	cuts    []span
	docs    []heredoc
	notes   *shell.Comments // built at the first `#`
}

// scan fills cuts and docs; false is a parse it cannot trust (backtick, unbalanced, unterminated), so nothing is cut.
func (s *codeScan) scan() bool {
	for i := 0; i < len(s.cmd); i++ {
		next, ok := s.step(i)
		if !ok {
			return false
		}
		i = next
	}
	return len(s.stack) == 0
}

func (s *codeScan) top() byte {
	if len(s.stack) == 0 {
		return 0
	}
	return s.stack[len(s.stack)-1]
}

func (s *codeScan) push(ctx byte, i int) {
	s.stack = append(s.stack, ctx)
	if ctx == '(' {
		s.segs = append(s.segs, i+1)
	}
}

func (s *codeScan) pop() {
	if s.top() == '(' {
		s.segs = s.segs[:len(s.segs)-1]
	}
	s.stack = s.stack[:len(s.stack)-1]
}

// step consumes the token at cmd[i] and returns its last index.
func (s *codeScan) step(i int) (int, bool) {
	c, rest := s.cmd[i], s.cmd[i:]
	switch {
	case c == '`':
		return i, false
	case s.top() == '"':
		return s.inDouble(i), true
	case c == '\\':
		return i + 1, true
	case c == '\'':
		end := strings.IndexByte(s.cmd[i+1:], '\'')
		return i + 1 + end, end >= 0
	case strings.HasPrefix(rest, "$'"):
		return ansiEnd(s.cmd, i+2)
	case c == '"':
		s.push('"', i)
	case strings.HasPrefix(rest, "$(") || strings.HasPrefix(rest, "${"):
		s.push(rest[1], i+1)
		return i + 1, true
	case s.top() == '{':
		if c == '}' {
			s.pop()
		}
	default:
		return s.codeStep(i)
	}
	return i, true
}

// codeStep is step in a code context: groups, comments, heredoc operators and line or segment ends.
func (s *codeScan) codeStep(i int) (int, bool) {
	c, rest := s.cmd[i], s.cmd[i:]
	switch {
	case c == '(':
		s.push('(', i)
	case c == ')':
		if s.top() != '(' {
			return i, false
		}
		s.pop()
	case c == '#' && s.commentAt(i):
		end := strings.IndexByte(rest, '\n')
		if end < 0 {
			end = len(rest)
		}
		s.cuts = append(s.cuts, span{i, i + end})
		return i + end - 1, true
	case strings.HasPrefix(rest, "<<<"):
		return i + 2, true
	case strings.HasPrefix(rest, "<<"):
		return s.heredocOp(i)
	case c == '\n':
		return s.lineEnd(i)
	case strings.IndexByte(";&|", c) >= 0:
		s.segs[len(s.segs)-1] = i + 1
	}
	return i, true
}

// commentAt reports a comment-starting `#` at cmd[i], indexing cmd once for every `#` it is asked about.
func (s *codeScan) commentAt(i int) bool {
	if s.notes == nil {
		c := shell.ScanComments(s.cmd)
		s.notes = &c
	}
	return s.notes.At(i)
}

func (s *codeScan) inDouble(i int) int {
	switch rest := s.cmd[i:]; {
	case rest[0] == '\\':
		return i + 1
	case rest[0] == '"':
		s.pop()
	case strings.HasPrefix(rest, "$(") || strings.HasPrefix(rest, "${"):
		s.push(rest[1], i+1)
		return i + 1
	}
	return i
}

// ansiEnd is the closing quote of a $'…' string whose body starts at i.
func ansiEnd(cmd string, i int) (int, bool) {
	for ; i < len(cmd); i++ {
		switch cmd[i] {
		case '\\':
			i++
		case '\'':
			return i, true
		}
	}
	return i, false
}

var heredocOpRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^<<(-?)[ \t]*(?:'([^'\n]*)'|"([^"\\$` + "`" + `\n]*)"|([A-Za-z_][A-Za-z0-9_]*))`)
})

// heredocOp queues the operator at cmd[i] for the next line end; a delimiter it cannot parse fails the scan.
func (s *codeScan) heredocOp(i int) (int, bool) {
	m := heredocOpRe().FindStringSubmatchIndex(s.cmd[i:])
	if m == nil || i+m[1] < len(s.cmd) && strings.IndexByte(" \t\n;&|)<>", s.cmd[i+m[1]]) < 0 {
		return i, false
	}
	g := func(n int) string { return s.cmd[max(i+m[2*n], i):max(i+m[2*n+1], i)] }
	s.pending = append(s.pending, heredoc{delim: g(2) + g(3) + g(4), dash: g(1) == "-", quoted: m[8] < 0,
		data: dataConsumer(s.cmd[s.segs[len(s.segs)-1]:i])})
	return i + m[1] - 1, true
}

// lineEnd ends a segment and skips the bodies of the line's heredocs; an unclosed body fails the scan.
func (s *codeScan) lineEnd(i int) (int, bool) {
	s.segs[len(s.segs)-1] = i + 1
	if len(s.pending) == 0 {
		return i, true
	}
	pos := i + 1
	for _, h := range s.pending {
		start := pos
		for {
			if pos > len(s.cmd) {
				return i, false
			}
			end := len(s.cmd)
			if k := strings.IndexByte(s.cmd[pos:], '\n'); k >= 0 {
				end = pos + k
			}
			line := s.cmd[pos:end]
			pos = end + 1
			if line == h.delim || h.dash && strings.TrimLeft(line, "\t") == h.delim {
				break
			}
		}
		h.span = span{start, pos - 1}
		body := s.cmd[start:h.to]
		h.data = h.data && (h.quoted || !strings.Contains(body, "$(") && !strings.Contains(body, "`")) // unquoted: $(…) runs
		s.docs = append(s.docs, h)
	}
	s.pending = nil
	return pos - 2, true
}

// dataConsumers read a heredoc as data: interpreters, readers and git/gh; a shell, a writer or a remote run is none.
var dataConsumers = sync.OnceValue(func() map[string]bool { return shell.GuardSet("heredoc_data_consumers") })

var pythonName = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^python[23]?(\.[0-9]+)?$`) })

// dataConsumer reports a segment (text before its `<<`) whose command reads its heredoc as data.
func dataConsumer(seg string) bool {
	toks, _ := shell.Unwrap(shell.Tokens(seg), "")
	return len(toks) > 0 && (dataConsumers()[toks[0]] || pythonName().MatchString(toks[0]))
}

func spansOf(docs []heredoc, dataOnly bool) []span {
	var out []span
	for _, d := range docs {
		if d.data || !dataOnly {
			out = append(out, d.span)
		}
	}
	return out
}

// without is s minus the non-overlapping spans.
func without(s string, cuts []span) string {
	slices.SortFunc(cuts, func(a, b span) int { return a.from - b.from })
	var b strings.Builder
	at := 0
	for _, c := range cuts {
		b.WriteString(s[at:c.from])
		at = c.to
	}
	b.WriteString(s[at:])
	return b.String()
}

// bodyRunners can run a heredoc's text (or what a consumer prints) as commands, or keep it in a file to run later.
var bodyRunners = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[\s;&|()<>"'=/{])(?:(?:ba|z|da|k|c|tc|a)?sh|fish|busybox|eval|source|` +
		`exec|xargs|parallel|ssh|su|sudo|doas|at|batch|crontab|tee|dd|cp|mv|install|chmod)(?:$|[\s;&|()<>"'}])` +
		`|\$[^(]|\$$|>(?:[^&]|$)|>&[^0-9-]`)
})

// runsBody reports code outside the bodies that may run them: a bodyRunners hit or a command named by path.
func runsBody(code string) bool {
	if bodyRunners().MatchString(code) {
		return true
	}
	outer, subs := shell.ExtractSubstitutions(code)
	for _, seg := range append(shell.Segments(outer), subs...) {
		for _, sub := range shell.Segments(seg) {
			if toks, _ := shell.Unwrap(shell.Tokens(sub), ""); len(toks) > 0 && (toks[0] == "." ||
				strings.ContainsAny(toks[0], "/~")) {
				return true
			}
		}
	}
	return false
}

// decideBash is the strictest verdict over every segment, substitution and `bash -c` body; redirects never Allow.
func (p Permissions) decideBash(cmd string, depth int, cwd string) Decision {
	if depth > 8 {
		return Deny
	}
	outer, subs := shell.ExtractSubstitutions(cmd)
	if len(subs) > 0 && (slices.Contains(p.Deny, ReadOnlyDeny[0]) || p.spliceReadDenied(cmd)) { // read-only never runs one
		return Deny
	}
	worst, seen, start := Allow, false, cwd
	for _, sub := range subs {
		worst = min(worst, p.decideBash(sub, depth+1, ""), Ask)
		cwd = "" // a masked substitution may sit in a cd target
	}
	segs, seps := shell.SegmentsSep(outer)
	moved := false // an earlier segment may have changed the cwd
	for i, seg := range segs {
		at := cwd
		if at == "" && !moved {
			at = start
		}
		next, cdTo := cdDir(seg, cwd)
		if cwd = next; seps[i] != "&&" { // only an `&&` after a static cd carries a known cwd to the next segment
			cwd = ""
		}
		if d, ok := p.decideSeg(seg, cdTo, opDir{at, moved && at == ""}, depth); ok {
			seen, worst = true, min(worst, d)
		}
		moved = moved || mayMoveCwd(seg)
	}
	if !seen { // a bare `$(x)` or backtick command: its substitutions' verdict still counts
		return min(worst, p.decideOne("Bash", strings.TrimSpace(cmd)))
	}
	return worst
}

var subOpener = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("[$<>]\\(|`") })

// spliceReadDenied: under a Read deny, a segment that opens a substitution may read a path no check can see.
func (p Permissions) spliceReadDenied(cmd string) bool {
	return slices.ContainsFunc(shell.Segments(cmd), func(seg string) bool {
		return subOpener().MatchString(seg) && readOnlyReadDenied(p, strings.ReplaceAll(seg, "`", "$"), opDir{lost: true}) // `x` is $(x)
	})
}

// commandWords reads `rtk x` as x, a path-named program by its basename and $HOME/${HOME}/home as ~.
func commandWords(toks []string) []string {
	if len(toks) > 0 && filepath.Base(toks[0]) == "rtk" {
		// `rtk read` and flag-only `rtk -h` are rtk's own words, not a program: left as typed for the read-only check
		if rest, _ := shell.Unwrap(shell.DropRtk(toks), ""); len(rest) > 0 && rest[0] != "read" {
			toks = rest
		}
	}
	out := slices.Clone(toks)
	if len(out) > 0 && strings.Contains(out[0], "/") {
		out[0] = filepath.Base(out[0])
	}
	home, _ := os.UserHomeDir()
	for i, t := range out {
		out[i] = tildeWord(t, home)
	}
	return out
}

func tildeWord(t, home string) string {
	for _, h := range []string{"${HOME}", "$HOME", home} {
		if h != "" && (t == h || len(t) > len(h) && strings.HasPrefix(t, h) && t[len(h)] == '/') {
			return "~" + t[len(h):]
		}
	}
	return t
}

// decideSeg is one segment's verdict, cdTo a relative cd's absolute target; heredocs and file redirects never Allow.
func (p Permissions) decideSeg(seg, cdTo string, at opDir, depth int) (Decision, bool) {
	rawToks := shell.Tokens(seg)
	if len(rawToks) == 0 { // a blank line, a trailing `;` or a masked substitution; `env` or `x=1` alone is judged
		return Allow, false
	}
	toks, _ := shell.Unwrap(rawToks, "")
	toks = commandWords(toks)
	d := p.decideSegment(strings.TrimSpace(seg), rawToks, toks)
	judged := seg
	if cdTo != "" {
		judged = "cd " + textutil.ShellQuote(cdTo)
		d = p.cdVerdict(d, strings.TrimSpace(seg), strings.Join(toks, " "), judged)
	}
	if nested, ok := shell.NestedCommand(toks); ok {
		d = min(d, p.decideBash(nested, depth+1, ""), Ask)
	}
	if d == Allow && (strings.Contains(seg, "<") || shell.FileRedirect(seg)) {
		d = Ask
	}
	if d == Allow && readOnlyReadDenied(p, judged, at) {
		d = Deny
	}
	return d, true
}

// mayMoveCwd reports a segment that may change the shell's cwd: any cd, pushd or popd word, or eval and source.
func mayMoveCwd(seg string) bool {
	return slices.ContainsFunc(shell.Tokens(seg), func(t string) bool {
		return slices.Contains(cwdMovingWords(), t)
	})
}

var cwdMovingWords = sync.OnceValue(func() []string { return shell.GuardWords("cwd_moving_words") })

// cdVerdict joins a relative cd's verdict d with its absolute spelling's: a deny of either wins, then an allow.
func (p Permissions) cdVerdict(d Decision, raw, norm, abs string) Decision {
	hit := func(rules []Rule) bool {
		return anyRule(rules, "Bash", raw, false) || anyRule(rules, "Bash", norm, true)
	}
	switch {
	case anyRule(p.Deny, "Bash", abs, false):
		return Deny
	case d == Allow || hit(p.Deny) || hit(p.Ask):
		return d
	}
	return max(d, p.decideOne("Bash", abs))
}

// cdDir is the cleaned absolute dir a bare `cd <static path>` enters from cwd; cdTo is it for a relative operand.
func cdDir(seg, cwd string) (dir, cdTo string) {
	toks := shell.Tokens(seg)
	if len(toks) != 2 || toks[0] != "cd" || strings.HasPrefix(toks[1], "-") || strings.ContainsAny(toks[1], "$`*?[{~\\") {
		return "", ""
	}
	if path.IsAbs(toks[1]) {
		return path.Clean(toks[1]), ""
	}
	if cwd = filepath.ToSlash(cwd); !path.IsAbs(cwd) {
		return "", ""
	}
	dir = path.Join(cwd, toks[1])
	return dir, dir
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
	}
	return p.fallback()
}

// fallback is the mode's verdict on a call no rule matched: allow allows, plan denies, the rest ask.
func (p Permissions) fallback() Decision {
	switch p.Mode {
	case PermAllow:
		return Allow
	case PermPlan:
		return Deny
	}
	return Ask
}

// SedPrintArg is the Bash allow argument for read-only `sed -n <range>p FILE...`; any other script or flag is no match.
const SedPrintArg = "sed -n N,Mp FILE"

var sedRange = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(\d+|\$)(,(\d+|\$))?p$`) })

// sedPrintMatches is `sed -n <range>p` with plain file operands: no -i, -e, -f, w/e/r commands or other option.
func sedPrintMatches(cmd string) bool {
	toks := shell.Tokens(cmd)
	if len(toks) < 4 || toks[0] != "sed" || toks[1] != "-n" || !sedRange().MatchString(toks[2]) {
		return false
	}
	return !slices.ContainsFunc(toks[3:], func(t string) bool { return strings.HasPrefix(t, "-") || strings.ContainsAny(t, "$`") })
}

const cdPrefix = "cd:"

// CdRule allows a segment that is exactly `cd <absolute path>` naming dir or a directory under it.
func CdRule(dir string) Rule { return Rule{Tool: "Bash", Arg: cdPrefix + dir} }

// cdMatches is a bare `cd <abs>` whose expansion-free target is dir or below it, cleaned or symlink-resolved.
func cdMatches(dir, cmd string) bool {
	toks := shell.Tokens(cmd)
	if len(toks) != 2 || toks[0] != "cd" {
		return false
	}
	operand := toks[1]
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(operand, "~/") { // commandWords spells $HOME as ~
		operand = filepath.ToSlash(home) + operand[1:]
	}
	if !path.IsAbs(operand) || strings.ContainsAny(operand, "$`*?[{~\\") {
		return false
	}
	target, clean := path.Clean(operand), path.Clean(dir)
	if target == clean || strings.HasPrefix(target, strings.TrimSuffix(clean, "/")+"/") {
		return true
	}
	return path.IsAbs(dir) && paths.Within(paths.CanonDeep(target), paths.CanonDeep(clean)) // /tmp is /private/tmp
}

var envAssign = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=`) })

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
		m := envAssign().FindStringSubmatch(t)
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
	}
	return p.fallback()
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
	return pathMatches(r.Arg, arg) || pathMatches(expandHome(tool, r.Arg), arg)
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

// pathMatches globs a path as written, then as canonical forms when both are absolute (/tmp equals /private/tmp).
func pathMatches(pattern, p string) bool {
	pattern, p = paths.CleanGlob(pattern), paths.CleanGlob(p)
	if paths.GlobMatch(pattern, p) {
		return true
	}
	return path.IsAbs(pattern) && path.IsAbs(p) && paths.GlobMatch(paths.CanonDeep(pattern), paths.CanonDeep(p))
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
		what = "--permissions plan"
	}
	return errors.New(agentName + " cannot enforce " + what + " yet; dispatch refused")
}
