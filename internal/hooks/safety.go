package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
)

// rmRecursive matches `rm` with any recursive flag spelling (-r, -Rf, -r -f, --recursive) and trailing flags.
const rmRecursive = `rm\s+(-\S+\s+)*(-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\s+(-\S+\s+)*`

// hardBlocks are command shapes that are almost never intentional.
var hardBlocks = lazyCompile(
	rmRecursive+`/(\*|\s|$|[)\x60"'])`,
	rmRecursive+`/(etc|usr|var|bin|sbin|lib|System|Users|home)(/?\s|/?$|/?[)\x60"'])`,
	rmRecursive+`(\$HOME|\$\{HOME\}|~)/?(\*|\s|$|[)\x60"'])`,
	`mkfs\.`,
	`dd\s+.*\sof=/dev/(sd|disk|nvme|hd)`,
	`chmod\s+-R\s+777\s+/(\s|$)`,
	`:\(\)\s*\{.*\};\s*:`,
	`python3?\s+.*-c\s+.*exec\(.*base64`,
)

// pipeBlocks match the WHOLE command text (minus inert data, see inertStripped) because pipes and encoded Python heredocs span shell segments.
var pipeBlocks = lazyCompile(
	`(^|[^[:alnum:]_])(curl|wget)\s[^|]*\|\s*(ba|z)?sh([^[:alnum:]_]|$)`,
	`(^|[\s;&|({/\x60])(bash|sh|zsh|dash|ksh)\s+(-\S*\s+)*-[a-zA-Z]*c[a-zA-Z]*\s+["']?(\$\(\s*|\x60\s*)(\S*/)?(curl|wget)\s`,
	`(^|[\s;&|({/\x60])(bash|sh|zsh|dash|ksh|source|\.)\s+(-\S*\s+)*(<\s*)?<\(\s*(\S*/)?(curl|wget)\s`,
	`(?s)(^|[\s;&|({/\x60])python3?(\.[0-9]+)?\b[^\n]*<<[-<]?.*((exec|eval)\(.*(base64|b64decode)|(base64|b64decode).*(exec|eval)\()`,
)

// askRule is an intent-dependent shape matched within one shell segment, with its message and alternative.
type askRule struct {
	what, hint string
	pats       func() []*regexp.Regexp
}

const gitPushPrefix = `git\s+((-[Cc]\s+|--(git-dir|work-tree)(=|\s+))("[^"]*"|'[^']*'|[^\s"']+)\s+)*push\s+`

var askRules = []askRule{
	{"sudo command", "Approve only if you asked for it; otherwise install to a user prefix (~/.local, pipx, a venv) instead.",
		lazyCompile(`(^|[^[:alnum:]_])sudo(\s|$)`)},
	{"force push to main/master", "Push a branch and open a PR instead: git push origin HEAD:<branch>.", lazyCompile(
		gitPushPrefix+`.*(--force|-f)(\s|$).*\b(main|master)(\s|$)`,
		gitPushPrefix+`.*\b(main|master)(\s|$).*(--force|-f)(\s|$)`,
		gitPushPrefix+`.*--force-with-lease(=[^[:space:]]*)?(\s|$).*\b(main|master)(\s|$)`,
		gitPushPrefix+`.*\b(main|master)(\s|$).*--force-with-lease(=[^[:space:]]*)?(\s|$)`,
		gitPushPrefix+`.*\+([^[:space:]]+:)?(refs/heads/)?\b(main|master)(\s|$)`,
	)},
	{"recursive removal of a .git directory",
		"That deletes the repository history; to start clean use a fresh worktree: claude-code worktree create <name>.",
		lazyCompile(`(^|[^[:alnum:]_])rm\s+(-\S+\s+)*(-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\s+(([^[:space:]"']+|"[^"]*"|'[^']*')\s+)*["']?([^[:space:]"']*/)?\.git/?["']?(\s|$)`)},
}

const terminalHint = "If intentional, run it yourself in a terminal."

// sqlDestructive runs against the lowercased command.
var sqlDestructive = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|[^[:alnum:]_])(drop\s+database|drop\s+table|truncate\s+table)([^[:alnum:]_]|$)`)
})

// exfilPatterns match credential-exfiltration shapes.
var exfilPatterns = lazyCompile(
	`(^|[\s;&|({/\x60'"\\])(env|printenv)['"]?\s*>\s*/tmp/`,
	`(cat|tail|head|base64)\s+.*(\.claude/settings\.json|\.credentials\.json|secrets\.env|\.ssh/|\.aws/credentials|\.netrc).*\|.*(^|[^[:alnum:]_])(curl|nc|ncat|wget)([^[:alnum:]_]|$)`,
	`base64\s+.*(\.ssh/id_|\.aws/credentials|\.credentials\.json|secrets\.env)`,
	`(scp|rsync)\s+.*\.ssh/id_`,
)

// softWarn patterns print a warning but never block.
var softWarn = lazyCompile(
	`kubectl\s+delete(\W|$)`,
	`docker\s+system\s+prune`,
	`git\s+reset\s+--hard`,
	`git\s+clean\s+-[a-z]*f`,
)

func lazyCompile(pats ...string) func() []*regexp.Regexp {
	return sync.OnceValue(func() []*regexp.Regexp {
		out := make([]*regexp.Regexp, len(pats))
		for i, p := range pats {
			out[i] = regexp.MustCompile(p)
		}
		return out
	})
}

// safetyScan checks command safety: block and non-Claude ask reject, a Claude ask prompts, soft warnings ride stderr.
func safetyScan(in *core.Input) core.Result {
	decision, msg, detail := safetyVerdict(in)
	var warn strings.Builder
	if decision != core.Block && (in.ToolName == "" || strings.EqualFold(in.ToolName, "Bash")) {
		for _, re := range softWarn() {
			if re.MatchString(in.ToolCommand()) {
				fmt.Fprintf(&warn, "safety: WARNING — %s (not blocked; review carefully)\n", re)
			}
		}
	}
	if decision == core.Allow { // an allowed verdict's message is a warning (the rewrite guard failing open)
		return core.Result{Stderr: strings.TrimSpace(msg + warn.String())}
	}
	if decision == core.Ask {
		detail = "ask: " + detail
	}
	audit.Log(audit.HookBlocked, "safety", detail)
	if decision == core.Ask && in.Agent() == core.AgentClaude {
		return core.Result{Decision: core.Ask, Reason: strings.TrimSpace(msg), Stderr: strings.TrimSpace(warn.String())}
	}
	return core.Result{Decision: core.Block, Reason: strings.TrimSpace(warn.String() + msg)}
}

// safetyVerdict checks all block patterns before considering any ask pattern.
func safetyVerdict(in *core.Input) (core.Decision, string, string) {
	if in.ToolName != "" && !strings.EqualFold(in.ToolName, "Bash") {
		return core.Allow, "", ""
	}
	cmd := in.ToolCommand()
	if cmd == "" {
		return core.Allow, "", ""
	}
	scan := inertStripped(cmd)
	for _, re := range pipeBlocks() {
		if re.MatchString(scan) {
			return core.Block, safetyMessage(in, core.Block, "destructive command", re.String(), terminalHint), re.String()
		}
	}
	if d, msg, detail, ok := hardBlockVerdict(in, cmd, scan); ok {
		return d, msg, detail
	}
	for _, re := range exfilPatterns() {
		if re.MatchString(scan) {
			return core.Block, safetyMessage(in, core.Block, "looks like credential exfiltration", re.String(), terminalHint), re.String()
		}
	}
	return safetyAskVerdict(in)
}

// safetyAskVerdict reports the first intent-dependent shape; the owner approves it on Claude, others block.
func safetyAskVerdict(in *core.Input) (core.Decision, string, string) {
	cmd := in.ToolCommand()
	// Each pattern is matched inside ONE shell segment: `git push origin main; readlink -f x` is not a force-push.
	for _, seg := range shellSegments(cmd) {
		for _, rule := range askRules {
			for _, re := range rule.pats() {
				if re.MatchString(seg) {
					return core.Ask, safetyMessage(in, core.Ask, rule.what, re.String(), rule.hint), re.String()
				}
			}
		}
	}
	d, msg, detail, warn := rewriteVerdict(in)
	if d == core.Allow {
		d, msg, detail = askPatternVerdict(in, cmd)
	}
	return d, warn + msg, detail
}

// askPatternVerdict asks on hard rule 4 git verbs, hard rule 3 footers and destructive SQL.
func askPatternVerdict(in *core.Input, cmd string) (core.Decision, string, string) {
	if harnessGitBlocked(cmd, in.CWD) {
		what := "git stash/checkout/switch/reset/restore/clean inside the shared config tree (CLAUDE.md hard rule 4)"
		hint := "Scope a build or diff your own files instead; commit early. Use a linked worktree instead: claude-code worktree create <name>."
		return core.Ask, safetyMessage(in, core.Ask, what, "", hint), "shared config git mutation"
	}
	if attributionFooterBlocked(cmd) {
		what := "AI-attribution footer in a commit message (CLAUDE.md hard rule 3)"
		hint := "Rerun this commit without the Co-Authored-By / Generated-with lines; that needs no approval."
		return core.Ask, safetyMessage(in, core.Ask, what, "", hint), "attribution footer"
	}
	if sqlDestructive().MatchString(strings.ToLower(inertStripped(cmd))) {
		hint := "Approve only if you asked for it; back up first or run it against a scratch database."
		return core.Ask, safetyMessage(in, core.Ask, "destructive SQL (DROP/TRUNCATE)", "", hint), "destructive SQL"
	}
	return core.Allow, "", ""
}

// safetyMessage renders the stderr/reason text; an ask that falls back to a block names the agent.
func safetyMessage(in *core.Input, decision core.Decision, what, pattern, hint string) string {
	word := "BLOCKED"
	if decision == core.Ask {
		if in.Agent() == core.AgentClaude {
			word = "ASK"
		} else {
			what += " (no ask decision on " + in.Agent() + ")"
		}
	}
	msg := fmt.Sprintf("safety: %s — %s\n", word, what)
	if pattern != "" {
		msg += "  pattern: " + pattern + "\n"
	}
	return msg + "  command: " + in.ToolCommand() + "\n" + hint + "\n"
}

// bannedGitVerbs are the tree-writing verbs hard rule 4 bans inside the shared config checkout.
var bannedGitVerbs = map[string]bool{"stash": true, "checkout": true, "reset": true, "restore": true, "clean": true, "switch": true}

// shellWrapperNames launch a nested command STRING (not further argv tokens) that needs its own check.
var shellWrapperNames = map[string]bool{"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true}

// envAssignRe matches a leading `NAME=value` token (e.g. `BASH_ENV=x`) prefixed to a command.
var envAssignRe = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
})

// skipEnvAssignments drops leading `NAME=value` tokens so the wrapper/eval check sees the real command.
func skipEnvAssignments(toks []string) []string {
	i := 0
	for i < len(toks) && envAssignRe().MatchString(toks[i]) {
		i++
	}
	return toks[i:]
}

// nestedShellCommand reports the command text a `bash -c "…"`/`eval "…"` token sequence would run.
func nestedShellCommand(toks []string) (string, bool) {
	toks = skipEnvAssignments(toks)
	if len(toks) == 0 {
		return "", false
	}
	if toks[0] == "eval" && len(toks) > 1 {
		return strings.Join(toks[1:], " "), true
	}
	if shellWrapperNames[filepath.Base(toks[0])] {
		for i := 1; i < len(toks)-1; i++ {
			t := toks[i]
			if len(t) > 1 && t[0] == '-' && t[1] != '-' && strings.ContainsRune(t[1:], 'c') {
				return toks[i+1], true
			}
		}
	}
	return "", false
}

// harnessGitBlocked reports a banned git verb whose target tree (--work-tree / --git-dir / -C chain, else the
// segment's cwd after any `cd`) sits under the config dir; shell segments and global options are parsed, not regexed.
func harnessGitBlocked(cmd, cwd string) bool {
	masked, subs := extractSubstitutions(dropRemoteHeredocs(cmd))
	for _, sub := range subs {
		if harnessGitBlocked(sub, cwd) { // a substitution's cd only affects its own nested evaluation
			return true
		}
	}
	var harness, worktrees string
	for _, seg := range shellSegments(masked) {
		if gitSegmentBlocked(seg, &cwd, &harness, &worktrees) {
			return true
		}
	}
	return false
}

// gitSegmentBlocked processes one shell segment: follows a `cd`'s cwd change and checks any git invocation.
func gitSegmentBlocked(seg string, cwd, harness, worktrees *string) bool {
	toks := shellTokens(seg)
	if len(toks) == 0 || toks[0] == "ssh" { // ssh runs its command on the remote host, not in this checkout
		return false
	}
	if next, ok := cdTarget(toks, *cwd); ok {
		*cwd = next
		return false
	}
	if nested, ok := nestedShellCommand(toks); ok && harnessGitBlocked(nested, *cwd) {
		return true
	}
	for i, t := range toks {
		if t != "git" { // any token: wrappers (env, command, timeout N) need no modelling
			continue
		}
		if gitTokenBlocked(toks[i+1:], *cwd, harness, worktrees) {
			return true
		}
	}
	return false
}

// gitTokenBlocked reports whether one `git <rest>` invocation targets a banned verb under the harness tree.
func gitTokenBlocked(rest []string, cwd string, harness, worktrees *string) bool {
	target, verb, args := gitInvocation(rest, cwd)
	if !bannedGitVerbs[verb] || (verb == "stash" && len(args) > 0 && (args[0] == "list" || args[0] == "show")) {
		return false
	}
	if *harness == "" {
		*harness = canonPath(paths.ConfigDir())
		*worktrees = canonPath(filepath.Join(paths.ConfigDir(), ".claude", "worktrees"))
	}
	tp := canonPath(target)
	if tp != *harness && !strings.HasPrefix(tp, *harness+string(filepath.Separator)) {
		return false
	}
	if strings.HasPrefix(tp, *worktrees+string(filepath.Separator)) {
		return false // inside a linked worktree: its own checkout, the live tree is untouched
	}
	return true
}

// cdTarget reports the cwd a `cd`/`pushd` segment leaves behind; a dynamic target keeps the cwd we had.
func cdTarget(toks []string, cwd string) (string, bool) {
	if toks[0] != "cd" && toks[0] != "pushd" {
		return cwd, false
	}
	if len(toks) == 1 {
		if home, err := os.UserHomeDir(); err == nil && toks[0] == "cd" {
			return home, true
		}
		return cwd, true
	}
	if p, ok := resolveShellPath(toks[1], cwd); ok {
		return p, true
	}
	return cwd, true
}

// resolveShellPath expands `~`/$HOME and anchors p to base; ok=false while the token stays dynamic.
func resolveShellPath(p, base string) (string, bool) {
	p = strings.Trim(p, `'"`)
	if p == "" || p == "-" {
		return base, false
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, pre := range []string{"~", "$HOME", "${HOME}"} {
			if p == pre || strings.HasPrefix(p, pre+"/") {
				p = home + p[len(pre):]
				break
			}
		}
	}
	if strings.ContainsAny(p, "$`*?") {
		return base, false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return p, true
}

// canonPath is absolute and symlink-resolved (or its parent is, for a path that does not exist yet).
func canonPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(r, filepath.Base(p))
	}
	return p
}

// dqEscapable are the bytes a backslash escapes inside POSIX double quotes; elsewhere it is literal.
func dqEscapable(c byte) bool {
	return c == '"' || c == '\\' || c == '$' || c == '`'
}

// extractSubstitutions pulls `$(...)` and backtick command substitutions out of cmd — including inside
// double quotes, where the shell evaluates both — leaving a placeholder space so neighboring tokens don't
// glue; single-quoted text is literal, so a substitution there is left untouched. Returns the bodies for
// the caller to check separately: they run as their own nested command, not inline in the outer segment.
func extractSubstitutions(s string) (string, []string) {
	var out strings.Builder
	var subs []string
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case escapedAt(s, i, quote):
			out.WriteByte(c)
			i++
			c = s[i]
		case c == '\'' && quote == 0: // inside "…" a ' is literal
			quote = '\''
		case c == '"':
			quote = toggleDoubleQuote(quote)
		default:
			if body, end, ok := substitutionAt(s, i); ok {
				subs = append(subs, body)
				i = end
				c = ' '
			}
		}
		out.WriteByte(c)
	}
	return out.String(), subs
}

// escapedAt reports a backslash at s[i] that escapes the next byte (always outside quotes, dqEscapable inside).
func escapedAt(s string, i int, quote byte) bool {
	return s[i] == '\\' && i+1 < len(s) && (quote == 0 || dqEscapable(s[i+1]))
}

// toggleDoubleQuote opens a double quote outside quotes and closes an open one.
func toggleDoubleQuote(quote byte) byte {
	if quote == '"' {
		return 0
	}
	return '"'
}

// substitutionAt reports a complete `$(…)` or backtick substitution starting at s[i]: its body and closing index.
func substitutionAt(s string, i int) (string, int, bool) {
	switch {
	case s[i] == '$' && i+1 < len(s) && s[i+1] == '(':
		if end := matchParen(s, i+1); end >= 0 {
			return s[i+2 : end], end, true
		}
	case s[i] == '`':
		if end := matchBacktick(s, i); end >= 0 {
			return s[i+1 : end], end, true
		}
	}
	return "", i, false
}

// quotedStep advances over s[i] inside quote: a double-quote escape consumes the next byte, the closing quote ends it.
func quotedStep(s string, i int, quote byte) (int, byte) {
	if s[i] == '\\' && quote == '"' && i+1 < len(s) && dqEscapable(s[i+1]) {
		return i + 1, quote
	}
	if s[i] == quote {
		return i, 0
	}
	return i, quote
}

// matchParen returns the index of the `)` matching the `(` at s[open], or -1; nested parens/quotes count.
func matchParen(s string, open int) int {
	depth, quote := 0, byte(0)
	for i := open; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			i, quote = quotedStep(s, i, quote)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// matchBacktick returns the index of the closing backtick matching s[start], or -1; quotes inside count.
func matchBacktick(s string, start int) int {
	quote := byte(0)
	for i := start + 1; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			i, quote = quotedStep(s, i, quote)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '\\':
			if i+1 < len(s) {
				i++
			}
		case '`':
			return i
		}
	}
	return -1
}

// shellSegments splits on unquoted command separators (`&&`, `||`, `;`, `|`, `&`, newline).
func shellSegments(cmd string) []string {
	segs, _ := shellSegmentsSep(cmd)
	return segs
}

// shellSegmentsSep is shellSegments plus the separator that ended each segment ("" for the last).
func shellSegmentsSep(cmd string) (out, seps []string) {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			j, q := quotedStep(cmd, i, quote)
			b.WriteString(cmd[i : j+1])
			i, quote = j, q
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
		case c == '\\' && i+1 < len(cmd):
			b.WriteByte(c)
			i++
			c = cmd[i]
		case strings.IndexByte(";|&\n", c) >= 0:
			sep, end := separatorAt(cmd, i)
			out, seps = append(out, b.String()), append(seps, sep)
			b.Reset()
			i = end
			continue
		}
		b.WriteByte(c)
	}
	return append(out, b.String()), append(seps, "")
}

// separatorAt is the separator starting at cmd[i] (one byte, or two when `|`/`&` follows) and its last index.
func separatorAt(cmd string, i int) (string, int) {
	if i+1 < len(cmd) && (cmd[i+1] == '|' || cmd[i+1] == '&') {
		return cmd[i : i+2], i + 1
	}
	return cmd[i : i+1], i
}

// dropRemoteHeredocs removes heredoc bodies fed to ssh: they run on the remote host, not in this checkout.
func dropRemoteHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		toks := shellTokens(lines[i])
		m := heredocOp.FindStringSubmatch(lines[i])
		if m == nil || len(toks) == 0 || toks[0] != "ssh" || strings.Contains(lines[i], "|") {
			continue
		}
		if end := heredocEnd(lines, i+1, m[2]+m[3]+m[4], m[1] == "-"); end >= 0 {
			i = end - 1
		}
	}
	return strings.Join(out, "\n")
}
