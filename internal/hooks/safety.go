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
	`(^|[^[:alnum:]_])sudo(\s|$)`,
	`git\s+push\s+.*(--force|-f)(\s|$).*(main|master)(\s|$)`,
	`git\s+push\s+.*(main|master)(\s|$).*(--force|-f)(\s|$)`,
	`python3?\s+.*-c\s+.*exec\(.*base64`,
)

// pipeBlocks span a pipe by design (a download piped into a shell), so they match the WHOLE command line.
var pipeBlocks = lazyCompile(
	`(^|[^[:alnum:]_])(curl|wget)\s[^|]*\|\s*(ba|z)?sh([^[:alnum:]_]|$)`,
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

// BashGuard applies safety before the optional Claude rtk rewrite.
func BashGuard(r io.Reader, stdout, stderr io.Writer) int {
	in := ParseInput(r)
	if code := safetyInput(in, stdout, stderr); code != ExitProceed {
		return code
	}
	return rtkWrap(in, stdout)
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
	if in.ToolName != "" && !strings.EqualFold(in.ToolName, "Bash") {
		return ExitProceed
	}
	cmd := in.ToolCommand()
	if cmd == "" {
		return ExitProceed
	}
	cmd = withoutHeredocBodies(cmd)
	for _, re := range pipeBlocks() {
		if re.MatchString(cmd) {
			fmt.Fprintf(stderr, "safety: BLOCKED — destructive command\n  pattern: %s\n  command: %s\nIf intentional, run it yourself in a terminal.\n", re, cmd)
			audit.Log(audit.HookBlocked, "safety", re.String())
			return ExitBlock
		}
	}
	// Each other pattern is matched inside ONE shell segment: `git push origin main; readlink -f x` is not a force-push.
	for _, seg := range shellSegments(cmd) {
		for _, re := range hardBlocks() {
			if re.MatchString(seg) {
				fmt.Fprintf(stderr, "safety: BLOCKED — destructive command\n  pattern: %s\n  command: %s\nIf intentional, run it yourself in a terminal.\n", re, cmd)
				audit.Log(audit.HookBlocked, "safety", re.String())
				return ExitBlock
			}
		}
	}
	if harnessGitBlocked(cmd, in.CWD) {
		fmt.Fprintf(stderr, "safety: BLOCKED — git stash/checkout/reset/restore/clean inside the shared config tree (CLAUDE.md hard rule 4)\n  command: %s\nScope a build or diff your own files instead; commit early.\n", cmd)
		audit.Log(audit.HookBlocked, "safety", "shared config git mutation")
		return ExitBlock
	}
	if sqlDestructive().MatchString(strings.ToLower(cmd)) {
		fmt.Fprintf(stderr, "safety: BLOCKED — destructive SQL (DROP/TRUNCATE)\n  command: %s\n", cmd)
		audit.Log(audit.HookBlocked, "safety", "destructive SQL")
		return ExitBlock
	}
	for _, re := range exfilPatterns() {
		if re.MatchString(cmd) {
			fmt.Fprintf(stderr, "safety: BLOCKED — looks like credential exfiltration\n  pattern: %s\n  command: %s\n", re, cmd)
			audit.Log(audit.HookBlocked, "safety", re.String())
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

// bannedGitVerbs are the tree-writing verbs hard rule 4 bans inside the shared config checkout.
var bannedGitVerbs = map[string]bool{"stash": true, "checkout": true, "reset": true, "restore": true, "clean": true}

// gitValueOpts are git GLOBAL options whose value is the next token when not spelled with `=`.
var gitValueOpts = map[string]bool{"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
	"--namespace": true, "--exec-path": true}

// harnessGitBlocked reports a banned git verb whose target tree (--work-tree / --git-dir / -C chain, else the
// segment's cwd after any `cd`) sits under the config dir; shell segments and global options are parsed, not regexed.
func harnessGitBlocked(cmd, cwd string) bool {
	var harness, worktrees string
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
			if harness == "" {
				harness = canonPath(paths.ConfigDir())
				worktrees = canonPath(filepath.Join(paths.ConfigDir(), ".claude", "worktrees"))
			}
			if tp := canonPath(target); tp == harness || strings.HasPrefix(tp, harness+string(filepath.Separator)) {
				if strings.HasPrefix(tp, worktrees+string(filepath.Separator)) {
					continue // inside a linked worktree: its own checkout, the live tree is untouched
				}
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
	cmd = withoutHeredocBodies(cmd)
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

// withoutHeredocBodies preserves command text while consuming queued heredocs in shell order.
func withoutHeredocBodies(cmd string) string {
	type heredoc struct {
		word string
		tabs bool
	}
	var pending []heredoc
	var out strings.Builder
	var quote byte
	for i := 0; i < len(cmd); {
		c := cmd[i]
		out.WriteByte(c)
		i++
		if c == '\\' && quote != '\'' && i < len(cmd) {
			out.WriteByte(cmd[i])
			i++
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '<' && i < len(cmd) && cmd[i] == '<' {
			start := i
			for i < len(cmd) && cmd[i] == '<' {
				i++
			}
			if i-start != 1 {
				out.WriteString(cmd[start:i])
				continue
			}
			h := heredoc{}
			if i < len(cmd) && cmd[i] == '-' {
				h.tabs = true
				i++
			}
			for i < len(cmd) && (cmd[i] == ' ' || cmd[i] == '\t') {
				i++
			}
			var word strings.Builder
			var q byte
			beg := i
			for i < len(cmd) {
				ch := cmd[i]
				if q == 0 && strings.ContainsRune(" \t\r\n;|&<>()", rune(ch)) {
					break
				}
				i++
				if ch == '\\' && q != '\'' && i < len(cmd) {
					word.WriteByte(cmd[i])
					i++
					continue
				}
				if q != 0 {
					if ch == q {
						q = 0
					} else {
						word.WriteByte(ch)
					}
				} else if ch == '\'' || ch == '"' {
					q = ch
				} else {
					word.WriteByte(ch)
				}
			}
			out.WriteString(cmd[start:i])
			if i > beg {
				h.word = word.String()
				pending = append(pending, h)
			}
		}
		if c == '\n' {
			for _, h := range pending {
				for i < len(cmd) {
					line := cmd[i:]
					if n := strings.IndexByte(line, '\n'); n >= 0 {
						line = line[:n]
						i += n + 1
					} else {
						i = len(cmd)
					}
					if h.tabs {
						line = strings.TrimLeft(line, "\t")
					}
					if line == h.word {
						out.WriteString(line + "\n") // the terminator stays, so a second pass is a no-op
						break
					}
				}
			}
			pending = nil
		}
	}
	return out.String()
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
