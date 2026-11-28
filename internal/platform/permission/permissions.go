package permission

import (
	"encoding/json"
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

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/shell"
	"github.com/edotau/claude-code/internal/platform/textutil"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
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

// canonicalTools are the tool names rules match on; a rule names one in any letter case.
var canonicalTools = []string{"Bash", "Edit", "Write", "MultiEdit", "NotebookEdit", "Read", "Glob", "Grep", "WebFetch",
	"WebSearch", "Agent", "Task"}

// CanonicalTool is name spelled as a known tool when it is one in another case (`write` is Write); else name.
func CanonicalTool(name string) string {
	if i := slices.IndexFunc(canonicalTools, func(t string) bool { return strings.EqualFold(t, name) }); i >= 0 {
		return canonicalTools[i]
	}
	return name
}

// UnmarshalJSON reads a rule from a policy document with its tool name canonical, as ParseRule does.
func (r *Rule) UnmarshalJSON(b []byte) error {
	type plain Rule
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*r = Rule{Tool: CanonicalTool(p.Tool), Arg: p.Arg}
	return nil
}

// Permissions is a dispatch's rule set; Mode ("", allow, default, auto, plan) decides what no rule matched.
type Permissions struct {
	Mode  string `json:"mode,omitempty"`
	Allow []Rule `json:"allow,omitempty"`
	Ask   []Rule `json:"ask,omitempty"`
	Deny  []Rule `json:"deny,omitempty"`
}

// String is the rule in ParseRule's syntax.
func (r Rule) String() string {
	if r.Arg == "" {
		return r.Tool
	}
	return r.Tool + "(" + r.Arg + ")"
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
		return Rule{Tool: CanonicalTool(s)}, nil
	}
	if !strings.HasSuffix(s, ")") {
		return Rule{}, fmt.Errorf("permission rule %q: unbalanced parentheses", s)
	}
	tool, arg := CanonicalTool(strings.TrimSpace(s[:open])), s[open+1:len(s)-1]
	switch {
	case tool == "":
		return Rule{}, fmt.Errorf("permission rule %q: empty tool name", s)
	case strings.TrimSpace(arg) == "":
		return Rule{}, fmt.Errorf("permission rule %q: empty argument (write %q for every call)", s, tool)
	}
	if strings.HasPrefix(arg, "~/") {
		arg = ExpandHome(tool, arg)
	}
	return Rule{Tool: tool, Arg: arg}, nil
}

// ExpandHome resolves home prefixes in path rules, retaining unknown users and failed lookups literally.
func ExpandHome(tool, arg string) string {
	if !strings.HasPrefix(arg, "~") || tool == "Bash" || tool == "WebFetch" {
		return arg
	}
	name, rest, _ := strings.Cut(arg[1:], "/")
	home, err := os.UserHomeDir()
	if name != "" {
		u, uerr := user.Lookup(name)
		if u != nil {
			home = u.HomeDir
		}
		err = uerr
	}
	if err != nil || home == "" {
		return arg
	}
	return filepath.Join(home, rest)
}

// PermAllow is the mode where every call runs unless a rule denies it; any other mode asks on no match.
const PermAllow = "allow"

// DecideAt applies Deny > Ask > Allow then the mode fallback; a static relative cd is judged as its absolute target.
func (p Permissions) DecideAt(cwd, tool, arg string) Decision {
	if tool != "Bash" {
		return p.decideForm(tool, arg, false)
	}
	code := shellCode(arg) // judge what bash runs: a comment can hide a quote that ends one
	return p.decideBash(code, 0, cwd)
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
	if len(cuts) == 0 {
		return s
	}
	slices.SortFunc(cuts, func(a, b span) int { return a.from - b.from })
	size := len(s)
	for _, c := range cuts {
		size -= c.to - c.from
	}
	var b strings.Builder
	b.Grow(size)
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
	worst, seen := Allow, false
	for _, sub := range subs {
		worst = min(worst, p.decideBash(sub, depth+1, ""), Ask)
		cwd = "" // a masked substitution may sit in a cd target
	}
	segs, seps := shell.SegmentsSep(outer)
	for i, seg := range segs {
		rawToks := shell.Tokens(seg)
		next, cdTo := cdDir(rawToks, cwd)
		if cwd = next; seps[i] != "&&" { // only an `&&` after a static cd carries a known cwd to the next segment
			cwd = ""
		}
		if d, ok := p.decideSeg(seg, rawToks, cdTo, depth); ok {
			seen, worst = true, min(worst, d)
		}
	}
	if !seen { // a bare `$(x)` or backtick command: its substitutions' verdict still counts
		return min(worst, p.decideForm("Bash", strings.TrimSpace(cmd), false))
	}
	return worst
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
func (p Permissions) decideSeg(seg string, rawToks []string, cdTo string, depth int) (Decision, bool) {
	if len(rawToks) == 0 { // a blank line, a trailing `;` or a masked substitution; `env` or `x=1` alone is judged
		if hasWrite(seg) { // `> f function a` tokenizes to nothing but still writes f
			return min(p.decideForm("Bash", strings.TrimSpace(seg), false), Ask), true
		}
		return Allow, false
	}
	raw, toks := strings.TrimSpace(seg), commandWords(UnwrapXargs(rawToks))
	norm := strings.Join(toks, " ")
	d := p.decideSegment(raw, norm, rawToks, toks)
	if cdTo != "" {
		d = p.cdVerdict(d, raw, norm, "cd "+textutil.ShellQuote(cdTo))
	}
	if nested, ok := shell.NestedCommand(toks); ok {
		d = min(d, p.decideBash(nested, depth+1, ""), Ask)
	}
	if d == Allow && hasWrite(seg) { // a quoted `<` is text, not a redirect
		d = Ask
	}
	return d, true
}

var xargsLongValueFlags = map[string]bool{
	"--max-args": true, "--max-procs": true, "--delimiter": true, "--max-chars": true, "--arg-file": true, "--process-slot-var": true,
}

// xargsOptionalShort are short options whose value is only ever attached (-i[R] -l[N] -e[E]).
const xargsOptionalShort = "ile"

// UnwrapXargs is toks past wrappers and any xargs, so the word left is the command run.
func UnwrapXargs(toks []string) []string {
	for {
		toks, _ = shell.Unwrap(toks, "")
		if len(toks) == 0 || filepath.Base(toks[0]) != "xargs" {
			return toks
		}
		toks = SkipXargs(toks)
	}
}

// SkipXargs is the words after a leading xargs and its options (values and `--` included).
func SkipXargs(toks []string) []string {
	values := shell.GuardWords("runner_value_flags")
	toks = toks[1:]
	for len(toks) > 0 && strings.HasPrefix(toks[0], "-") && toks[0] != "-" {
		opt := toks[0]
		toks = toks[1:]
		if opt == "--" {
			break
		}
		if xargsTakesNext(opt, values) && len(toks) > 0 {
			toks = toks[1:]
		}
	}
	return toks
}

// xargsTakesNext reports an option whose value is the next word, reading a short cluster like getopt.
func xargsTakesNext(opt string, values []string) bool {
	if strings.HasPrefix(opt, "--") {
		return xargsLongValueFlags[opt]
	}
	for i := 1; i < len(opt); i++ {
		if strings.IndexByte(xargsOptionalShort, opt[i]) >= 0 {
			return false
		}
		if slices.Contains(values, "-"+opt[i:i+1]) {
			return i == len(opt)-1
		}
	}
	return false
}

// cdVerdict joins a relative cd's verdict d with its absolute spelling's: a deny of either wins, then an allow.
func (p Permissions) cdVerdict(d Decision, raw, norm, abs string) Decision {
	switch {
	case anyRule(p.Deny, "Bash", abs, false):
		return Deny
	case d == Allow || bashHit(p.Deny, raw, norm) || bashHit(p.Ask, raw, norm):
		return d
	}
	return max(d, p.decideForm("Bash", abs, false))
}

// bashHit reports a rule matching a Bash segment's raw or normalized spelling.
func bashHit(rules []Rule, raw, norm string) bool {
	return anyRule(rules, "Bash", raw, false) || anyRule(rules, "Bash", norm, true)
}

// cdDir is the cleaned absolute dir a bare `cd <static path>` enters from cwd; cdTo is it for a relative operand.
func cdDir(toks []string, cwd string) (dir, cdTo string) {
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
func (p Permissions) decideSegment(raw, norm string, rawToks, toks []string) Decision {
	if !grantsNormalized(rawToks, toks) {
		return min(p.decideForm("Bash", raw, false), p.decideForm("Bash", norm, true))
	}
	switch {
	case bashHit(p.Deny, raw, norm):
		return Deny
	case bashHit(p.Ask, raw, norm):
		return Ask
	case bashHit(p.Allow, raw, norm):
		return Allow
	}
	return p.fallback()
}

// fallback is the mode's verdict on a call no rule matched: allow allows, the rest ask.
func (p Permissions) fallback() Decision {
	if p.Mode == PermAllow {
		return Allow
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

// decideForm is one call's verdict, a Bash command spelled raw or normalized (quotes stripped, wrappers dropped).
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
		if r.matches(tool, arg) && (!norm || r.Arg != RedirectFileArg) {
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
	if pathMatches(r.Arg, arg) {
		return true
	}
	exp := ExpandHome(tool, r.Arg)
	return exp != r.Arg && pathMatches(exp, arg)
}

// shellRuleForms are the Bash rule arguments that name a shell-aware check instead of a command spelling.
var shellRuleForms = sync.OnceValue(func() map[string]func(string) bool {
	return map[string]func(string) bool{
		RedirectFileArg: shell.FileRedirect,
		SedPrintArg:     sedPrintMatches,
	}
})

// commandMatches: "p:*" is a prefix at a word boundary, anything else must equal the command.
func commandMatches(pattern, cmd string) bool {
	if form, ok := shellRuleForms()[pattern]; ok { // the shell-aware rule forms first
		return form(cmd)
	}
	if dir, ok := strings.CutPrefix(pattern, cdPrefix); ok {
		return cdMatches(dir, cmd)
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
