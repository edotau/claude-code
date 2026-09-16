package hooks

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

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
	`(^|[^[:alnum:]_])sudo(\s|$)`,
	`git\s+push\s+.*(--force|-f)(\s|$).*(main|master)(\s|$)`,
	`git\s+push\s+.*(main|master)(\s|$).*(--force|-f)(\s|$)`,
	`(^|[^[:alnum:]_])(curl|wget)\s[^|]*\|\s*(ba|z)?sh([^[:alnum:]_]|$)`,
	`python3?\s+.*-c\s+.*exec\(.*base64`,
)

// sqlDestructive runs against the lowercased command.
var sqlDestructive = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|[^[:alnum:]_])(drop\s+database|drop\s+table|truncate\s+table)([^[:alnum:]_]|$)`)
})

// exfilPatterns match credential-exfiltration shapes.
var exfilPatterns = lazyCompile(
	`(env|printenv)\s*>\s*/tmp/`,
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

// Safety is the PreToolUse(Bash) guard: block destructive and exfiltration commands.
func Safety(r io.Reader, stderr io.Writer) int {
	in := ParseInput(r)
	if in.ToolName != "" && in.ToolName != "Bash" {
		return ExitProceed
	}
	cmd := in.ToolCommand()
	if cmd == "" {
		return ExitProceed
	}
	for _, re := range hardBlocks() {
		if re.MatchString(cmd) {
			fmt.Fprintf(stderr, "safety: BLOCKED — destructive command\n  pattern: %s\n  command: %s\nIf intentional, run it yourself in a terminal.\n", re, cmd)
			return ExitBlock
		}
	}
	if harnessGitBlocked(cmd, in.CWD) {
		fmt.Fprintf(stderr, "safety: BLOCKED — git stash/checkout/reset/restore/clean inside the shared config tree (CLAUDE.md hard rule 5)\n  command: %s\nScope a build or diff your own files instead; commit early.\n", cmd)
		return ExitBlock
	}
	if sqlDestructive().MatchString(strings.ToLower(cmd)) {
		fmt.Fprintf(stderr, "safety: BLOCKED — destructive SQL (DROP/TRUNCATE)\n  command: %s\n", cmd)
		return ExitBlock
	}
	for _, re := range exfilPatterns() {
		if re.MatchString(cmd) {
			fmt.Fprintf(stderr, "safety: BLOCKED — looks like credential exfiltration\n  pattern: %s\n  command: %s\n", re, cmd)
			return ExitBlock
		}
	}
	for _, re := range softWarn() {
		if re.MatchString(cmd) {
			fmt.Fprintf(stderr, "safety: WARNING — %s (not blocked; review carefully)\n", re)
		}
	}
	return ExitProceed
}

// bannedGitVerbs are the tree-writing verbs hard rule 5 bans inside the shared config checkout.
var bannedGitVerbs = map[string]bool{"stash": true, "checkout": true, "reset": true, "restore": true, "clean": true}

// gitValueOpts are git GLOBAL options whose value is the next token when not spelled with `=`.
var gitValueOpts = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true}

// harnessGitBlocked reports a banned git verb whose target tree (--work-tree / --git-dir / -C chain, else the
// segment's cwd after any `cd`) sits under the config dir; shell segments and global options are parsed, not regexed.
func harnessGitBlocked(cmd, cwd string) bool {
	harness := canonPath(paths.ConfigDir())
	for _, seg := range shellSegments(cmd) {
		toks := shellTokens(seg)
		if len(toks) == 0 {
			continue
		}
		if next, ok := cdTarget(toks, cwd); ok {
			cwd = next
			continue
		}
		for i, t := range toks {
			if t != "git" { // any token: wrappers (env, command, timeout N) need no modelling
				continue
			}
			target, verb, args := gitInvocation(toks[i+1:], cwd)
			if !bannedGitVerbs[verb] || (verb == "stash" && len(args) > 0 && (args[0] == "list" || args[0] == "show")) {
				continue
			}
			if tp := canonPath(target); tp == harness || strings.HasPrefix(tp, harness+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
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
		} else if strings.HasPrefix(t, "-C") {
			name, val = "-C", strings.TrimPrefix(t, "-C")
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

// shellSegments splits on unquoted command separators (`&&`, `||`, `;`, `|`, `&`, newline).
func shellSegments(cmd string) []string {
	var out []string
	var b strings.Builder
	var quote byte
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
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
		default:
			b.WriteByte(c)
			started = true
		}
	}
	flush()
	if len(out) > 0 {
		if out[0] = strings.TrimLeft(out[0], "({"); out[0] == "" {
			out = out[1:]
		}
	}
	return out
}
