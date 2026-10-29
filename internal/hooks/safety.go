package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/paths"
)

// hardBlocks are command shapes that are almost never intentional.
var hardBlocks = lazyCompile(
	`rm\s+-(rf|fr|r)\s+/(\*|\s|$)`,
	`rm\s+-(rf|fr|r)\s+/(etc|usr|var|bin|sbin|lib|System|Users|home)(/?\s|/?$)`,
	`rm\s+-(rf|fr|r)\s+(\$HOME|\$\{HOME\}|~)/?(\*|\s|$)`,
	`mkfs\.`,
	`dd\s+.*\sof=/dev/(sd|disk|nvme|hd)`,
	`chmod\s+-R\s+777\s+/(\s|$)`,
	`:\(\)\s*\{.*\};\s*:`,
	`python3?\s+.*-c\s+.*exec\(.*base64`,
)

// pipeBlocks match the WHOLE command text because pipes and encoded Python heredocs span shell segments.
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
		gitPushPrefix+`.*(--force|-f)(\s|$).*(main|master)(\s|$)`,
		gitPushPrefix+`.*(main|master)(\s|$).*(--force|-f)(\s|$)`,
		gitPushPrefix+`.*--force-with-lease(=[^[:space:]]*)?(\s|$).*(main|master)(\s|$)`,
		gitPushPrefix+`.*(main|master)(\s|$).*--force-with-lease(=[^[:space:]]*)?(\s|$)`,
		gitPushPrefix+`.*\+([^[:space:]]+:)?(refs/heads/)?(main|master)(\s|$)`,
	)},
	{"recursive removal of a .git directory",
		"That deletes the repository history; to start clean use a fresh worktree: claude-code worktree create <name>.",
		lazyCompile(`(^|[^[:alnum:]_])rm\s+(-[a-zA-Z]+\s+)*-[a-zA-Z]*[rR][a-zA-Z]*\s+(([^[:space:]"']+|"[^"]*"|'[^']*')\s+)*["']?([^[:space:]"']*/)?\.git/?["']?(\s|$)`)},
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

// SafetyScan checks command safety.
func SafetyScan(r io.Reader, stdout, stderr io.Writer) int {
	return safetyInput(ParseInput(r), stdout, stderr)
}

func safetyInput(in *Input, stdout, stderr io.Writer) (code int) {
	if in.Vendor() == VendorCopilot {
		var reason strings.Builder
		stderr = io.MultiWriter(stderr, &reason)
		defer func() {
			if code == ExitBlock {
				_ = json.NewEncoder(stdout).Encode(map[string]string{
					"permissionDecision": "deny", "permissionDecisionReason": strings.TrimSpace(reason.String()),
				})
			}
		}()
	}
	decision, msg, detail := safetyVerdict(in)
	if decision != Block && (in.ToolName == "" || strings.EqualFold(in.ToolName, "Bash")) {
		for _, re := range softWarn() {
			if re.MatchString(in.ToolCommand()) {
				fmt.Fprintf(stderr, "safety: WARNING — %s (not blocked; review carefully)\n", re)
			}
		}
	}
	if decision == Allow {
		return ExitProceed
	}
	if decision == Ask {
		detail = "ask: " + detail
	}
	audit.Log(audit.HookBlocked, "safety", detail)
	if decision == Ask && in.Vendor() == VendorClaude {
		fmt.Fprintln(stdout, OutputClaude("PreToolUse", Result{Decision: Ask, Reason: strings.TrimSpace(msg)}))
		return ExitProceed
	}
	fmt.Fprint(stderr, msg)
	return ExitBlock
}

// safetyVerdict checks all block patterns before considering any ask pattern.
func safetyVerdict(in *Input) (Decision, string, string) {
	if in.ToolName != "" && !strings.EqualFold(in.ToolName, "Bash") {
		return Allow, "", ""
	}
	cmd := in.ToolCommand()
	if cmd == "" {
		return Allow, "", ""
	}
	for _, re := range pipeBlocks() {
		if re.MatchString(cmd) {
			return Block, safetyMessage(in, Block, "destructive command", re.String(), terminalHint), re.String()
		}
	}
	for _, seg := range shellSegments(cmd) {
		for _, re := range hardBlocks() {
			if re.MatchString(seg) {
				return Block, safetyMessage(in, Block, "destructive command", re.String(), terminalHint), re.String()
			}
		}
	}
	for _, re := range exfilPatterns() {
		if re.MatchString(cmd) {
			return Block, safetyMessage(in, Block, "looks like credential exfiltration", re.String(), terminalHint), re.String()
		}
	}
	return safetyAskVerdict(in)
}

// safetyAskVerdict reports the first intent-dependent shape; the owner approves it on Claude, others block.
func safetyAskVerdict(in *Input) (Decision, string, string) {
	cmd := in.ToolCommand()
	// Each pattern is matched inside ONE shell segment: `git push origin main; readlink -f x` is not a force-push.
	for _, seg := range shellSegments(cmd) {
		for _, rule := range askRules {
			for _, re := range rule.pats() {
				if re.MatchString(seg) {
					return Ask, safetyMessage(in, Ask, rule.what, re.String(), rule.hint), re.String()
				}
			}
		}
	}
	if harnessGitBlocked(cmd, in.CWD) {
		what := "git stash/checkout/reset/restore/clean inside the shared config tree (CLAUDE.md hard rule 4)"
		hint := "Scope a build or diff your own files instead; commit early. Use a linked worktree instead: claude-code worktree create <name>."
		return Ask, safetyMessage(in, Ask, what, "", hint), "shared config git mutation"
	}
	if attributionFooterBlocked(cmd) {
		what := "AI-attribution footer in a commit message (CLAUDE.md hard rule 3)"
		hint := "Drop the Co-Authored-By / Generated-with lines; the committer identity covers it."
		return Ask, safetyMessage(in, Ask, what, "", hint), "attribution footer"
	}
	if sqlDestructive().MatchString(strings.ToLower(cmd)) {
		hint := "Approve only if you asked for it; back up first or run it against a scratch database."
		return Ask, safetyMessage(in, Ask, "destructive SQL (DROP/TRUNCATE)", "", hint), "destructive SQL"
	}
	return Allow, "", ""
}

// safetyMessage renders the stderr/reason text; an ask that falls back to a block names the vendor.
func safetyMessage(in *Input, decision Decision, what, pattern, hint string) string {
	word := "BLOCKED"
	if decision == Ask {
		if in.Vendor() == VendorClaude {
			word = "ASK"
		} else {
			what += " (no ask decision on " + in.Vendor() + ")"
		}
	}
	msg := fmt.Sprintf("safety: %s — %s\n", word, what)
	if pattern != "" {
		msg += "  pattern: " + pattern + "\n"
	}
	return msg + "  command: " + in.ToolCommand() + "\n" + hint + "\n"
}

// bannedGitVerbs are the tree-writing verbs hard rule 4 bans inside the shared config checkout.
var bannedGitVerbs = map[string]bool{"stash": true, "checkout": true, "reset": true, "restore": true, "clean": true}

// gitValueOpts are git GLOBAL options whose value is the next token when not spelled with `=`.
var gitValueOpts = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true}

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
	masked, subs := extractSubstitutions(cmd)
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
	if len(toks) == 0 {
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

// gitInvocation parses git's global options up to the verb: the target tree, the verb, the verb's words.
func gitInvocation(toks []string, cwd string) (target, verb string, args []string) {
	var cPaths []string
	var gitDir, workTree string
	i := 0
	for i < len(toks) && strings.HasPrefix(toks[i], "-") {
		t := toks[i]
		i++
		name, val := t, ""
		if n, v, ok := strings.Cut(t, "="); ok {
			name, val = n, v
		} else if gitValueOpts[t] {
			if i < len(toks) {
				val = toks[i]
				i++
			}
		} else if after, ok0 := strings.CutPrefix(t, "-C"); ok0 {
			name, val = "-C", after
		} else {
			continue // valueless global option: --no-pager, --bare…
		}
		switch name {
		case "-C":
			cPaths = append(cPaths, val)
		case "--git-dir":
			gitDir = val
		case "--work-tree":
			workTree = val
		}
	}
	if i < len(toks) {
		verb, args = toks[i], toks[i+1:]
	}
	target = cwd
	if p, ok := resolveShellPath(workTree, cwd); workTree != "" && ok {
		return p, verb, args
	}
	if p, ok := resolveShellPath(gitDir, cwd); gitDir != "" && ok && filepath.Base(p) == ".git" {
		return filepath.Dir(p), verb, args
	}
	for _, c := range cPaths { // successive -C paths apply relative to each other
		if p, ok := resolveShellPath(c, target); ok {
			target = p
		}
	}
	return target, verb, args
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
		if quote == '\'' {
			out.WriteByte(c)
			if c == '\'' {
				quote = 0
			}
			continue
		}
		if c == '\\' && i+1 < len(s) && (quote == 0 || dqEscapable(s[i+1])) {
			out.WriteByte(c)
			i++
			out.WriteByte(s[i])
			continue
		}
		switch {
		case c == '\'':
			quote = '\''
			out.WriteByte(c)
		case c == '"':
			if quote == '"' {
				quote = 0
			} else {
				quote = '"'
			}
			out.WriteByte(c)
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			if end := matchParen(s, i+1); end >= 0 {
				subs = append(subs, s[i+2:end])
				i = end
				out.WriteByte(' ')
				continue
			}
			out.WriteByte(c)
		case c == '`':
			if end := matchBacktick(s, i); end >= 0 {
				subs = append(subs, s[i+1:end])
				i = end
				out.WriteByte(' ')
				continue
			}
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	return out.String(), subs
}

// matchParen returns the index of the `)` matching the `(` at s[open], or -1; nested parens/quotes count.
func matchParen(s string, open int) int {
	depth, quote := 0, byte(0)
	for i := open; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote == '"' && i+1 < len(s) && dqEscapable(s[i+1]) {
				i++
			} else if c == quote {
				quote = 0
			}
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
			if c == '\\' && quote == '"' && i+1 < len(s) && dqEscapable(s[i+1]) {
				i++
			} else if c == quote {
				quote = 0
			}
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
	var out []string
	var b strings.Builder
	var quote byte
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			if quote == '"' && c == '\\' && i+1 < len(cmd) && dqEscapable(cmd[i+1]) {
				b.WriteByte(c)
				i++
				b.WriteByte(cmd[i])
				continue
			}
			if c == quote {
				quote = 0
			}
			b.WriteByte(c)
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
			b.WriteByte(c)
		case c == '\\' && i+1 < len(cmd):
			b.WriteByte(c)
			i++
			b.WriteByte(cmd[i])
		case c == ';' || c == '|' || c == '&' || c == '\n':
			if i+1 < len(cmd) && (cmd[i+1] == '|' || cmd[i+1] == '&') {
				i++
			}
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteByte(c)
		}
	}
	return append(out, b.String())
}

// shellTokens splits one segment into unquoted words, dropping a leading `(`/`{` so a subshell reads as its segment.
func shellTokens(seg string) []string {
	var out []string
	var b strings.Builder
	var quote byte
	started := false
	flush := func() {
		if started {
			out = append(out, b.String())
			b.Reset()
			started = false
		}
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if quote != 0 {
			if quote == '"' && c == '\\' && i+1 < len(seg) && dqEscapable(seg[i+1]) {
				i++
				b.WriteByte(seg[i])
				continue
			}
			if c == quote {
				quote = 0
			} else {
				b.WriteByte(c)
			}
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
			started = true
		case c == '\\' && i+1 < len(seg):
			i++
			b.WriteByte(seg[i])
			started = true
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			flush()
		case c == ')' || c == '}':
			flush() // unquoted close of a subshell/group: a word boundary, never part of the word
		default:
			b.WriteByte(c)
			started = true
		}
	}
	flush()
	if len(out) > 0 {
		out[0] = strings.TrimLeft(out[0], "({")
		if out[0] == "" {
			out = out[1:]
		}
	}
	return out
}
