// Package safety is the command-safety hook area: safety-scan, permission-gate and the rtk rewrite.
package safety

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

// rmRecursive matches `rm` with any recursive flag spelling (-r, -Rf, -r -f, --recursive) and trailing operands.
const rmRecursive = `(^|[^[:alnum:]_])rm\s+(-\S+\s+)*(-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\s+(\S+\s+)*`

// hardBlocks are command shapes that are almost never intentional.
var hardBlocks = core.LazyCompile(
	rmRecursive+`["']?/(\*|\s|$|[)\x60"'])`,
	rmRecursive+`["']?/(etc|usr|var|bin|sbin|lib|lib64|boot|opt|root|System|Users|home|Applications|Library|Volumes|private)(/?\s|/?$|/?[)\x60"'])`,
	rmRecursive+`["']?(\$HOME|\$\{HOME\}|~)/?(\*|\s|$|[)\x60"'])`,
	`mkfs\.`,
	`dd\s+(.*\s)?of=/dev/r?(sd|disk|nvme|hd)`,
	`chmod\s+-R\s+0?777\s+/(\s|$)`,
	`python3?\s+.*-c\s+.*exec\(.*base64`,
)

// pipeBlocks match the WHOLE command text (minus inert data, see inertStripped): pipes, a fork bomb and encoded Python
// heredocs span shell segments.
var pipeBlocks = core.LazyCompile(
	`(^|[^[:alnum:]_])(curl|wget)\s[^|]*\|\s*(sudo\s+)?(\S*/)?(ba|z)?sh([^[:alnum:]_]|$)`,
	`(^|[\s;&|({/\x60])eval\s+["']?(\$\(|\x60)\s*(\S*/)?(curl|wget)\s`,
	`(^|[^[:alnum:]_])base64\s+(-d|-D|--decode)[^|]*\|\s*(sudo\s+)?(\S*/)?(ba|z|da|k)?sh([^[:alnum:]_]|$)`,
	`:\(\)\s*\{[^}]*\}\s*;\s*:`,
	`(^|[\s;&|({/\x60])(bash|sh|zsh|dash|ksh)\s+(-\S*\s+)*-[a-zA-Z]*c[a-zA-Z]*\s+["']?(\$\(\s*|\x60\s*)(\S*/)?(curl|wget)\s`,
	`(^|[\s;&|({/\x60])(bash|sh|zsh|dash|ksh|source|\.)\s+(-\S*\s+)*(<\s*)?<\(\s*(\S*/)?(curl|wget)\s`,
	`(?s)(^|[\s;&|({/\x60])python3?(\.[0-9]+)?\b[^\n]*<<[-<]?.*((exec|eval)\(.*(base64|b64decode)|(base64|b64decode).*(exec|eval)\()`,
)

// askRule is an intent-dependent shape matched within one shell segment, with its message and alternative.
type askRule struct {
	what, hint string
	pats       func() []*regexp.Regexp
}

const gitPushPrefix = `git\s+(((-[Cc]\s+|--(git-dir|work-tree)(=|\s+))("[^"]*"|'[^']*'|[^\s"']+)|--?[a-zA-Z][a-zA-Z-]*(=\S+)?)\s+)*push\s+`

// forceFlag is a standalone --force or short-flag cluster containing f (-f, -fu, -uf).
const forceFlag = `(--force|-[a-zA-Z]*f[a-zA-Z]*)(\s|$)`

// protectedBranch is main/master as a whole refspec word: after space, a quote, `:` or refs/heads/ (not feature-main)
const protectedBranch = `(.*[\s"':]|.*refs/heads/)?(main|master)["']?(\s|$)`

const (
	forcePushWhat = "force push to main/master"
	forcePushHint = "Push a branch and open a PR instead: git push origin HEAD:<branch>."
)

var askRules = []askRule{
	{"sudo command", "Approve only if you asked for it; otherwise install to a user prefix (~/.local, pipx, a venv) instead.",
		core.LazyCompile(`(^|[^[:alnum:]_])sudo(\s|$)`)},
	{forcePushWhat, forcePushHint, core.LazyCompile(
		gitPushPrefix+`(.*\s)?`+forceFlag+protectedBranch,
		gitPushPrefix+protectedBranch+`(.*\s)?`+forceFlag,
		gitPushPrefix+`.*--force-with-lease(=[^[:space:]]*)?(\s|$)`+protectedBranch,
		gitPushPrefix+protectedBranch+`.*--force-with-lease(=[^[:space:]]*)?(\s|$)`,
		gitPushPrefix+`.*\+([^[:space:]]+:)?(refs/heads/)?(main|master)["']?(\s|$)`,
	)},
	{"recursive removal of a .git directory",
		"That deletes the repository history; to start clean use a fresh worktree: claude-code worktree create <name>.",
		core.LazyCompile(`(^|[^[:alnum:]_])rm\s+(-\S+\s+)*(-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\s+(([^[:space:]"']+|"[^"]*"|'[^']*')\s+)*["']?([^[:space:]"']*/)?\.git/?["']?(\s|$)`)},
}

const terminalHint = "If intentional, run it yourself in a terminal."

// sqlDestructive runs against the lowercased command.
var sqlDestructive = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|[^[:alnum:]_])(drop\s+(database|table|schema)|truncate\s+table|truncate\s+[a-z_"][a-z0-9_."]*\s*(;|$|cascade|["']))([^[:alnum:]_]|$)`)
})

// exfilPatterns match credential-exfiltration shapes.
var exfilPatterns = core.LazyCompile(
	`(^|[\s;&|({/\x60'"\\])(env|printenv)['"]?\s*>\s*/tmp/`,
	`(cat|tail|head|base64)\s+.*(\.claude/settings\.json|\.credentials\.json|secrets\.env|\.ssh/|\.aws/credentials|\.netrc).*\|.*(^|[^[:alnum:]_])(curl|nc|ncat|wget)([^[:alnum:]_]|$)`,
	`base64\s+.*(\.ssh/id_|\.aws/credentials|\.credentials\.json|secrets\.env)`,
	`(scp|rsync)\s+.*\.ssh/id_`,
)

// softWarn patterns print a warning but never block.
var softWarn = core.LazyCompile(
	`kubectl\s+delete(\W|$)`,
	`docker\s+system\s+prune`,
	`git\s+reset\s+--hard`,
	`git\s+clean\s+-[a-z]*f`,
)

// Scan checks command safety: block and non-Claude ask reject, a Claude ask prompts, soft warnings ride stderr.
func Scan(in *core.Input) core.Result {
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
			return core.Block, core.SafetyMessage(in, core.Block, "destructive command", re.String(), terminalHint), re.String()
		}
	}
	d, msg, detail := hardBlockVerdict(in, cmd, scan)
	if d == core.Block {
		return d, msg, detail
	}
	for _, re := range exfilPatterns() {
		if re.MatchString(scan) {
			return core.Block, core.SafetyMessage(in, core.Block, "looks like credential exfiltration", re.String(), terminalHint), re.String()
		}
	}
	ask, askMsg, askDetail := safetyAskVerdict(in, scan)
	if d > ask {
		return d, askMsg + msg, detail
	}
	return ask, askMsg, askDetail
}

// safetyAskVerdict reports the first intent-dependent shape; the owner approves it on Claude, others block.
func safetyAskVerdict(in *core.Input, scan string) (core.Decision, string, string) {
	cmd := in.ToolCommand()
	// The rewrite block runs first: an ask shape (`sudo git reset --hard`) must not turn a block into an ask.
	d, msg, detail, warn := rewriteVerdict(in)
	if d != core.Allow {
		return d, warn + msg, detail
	}
	// Each pattern is matched inside ONE shell segment: `git push origin main; readlink -f x` is not a force-push.
	// Inert text (a commit message mentioning sudo) is blanked first; heredoc bodies stay, as for the hard blocks.
	for _, seg := range shell.Segments(blankInert(cmd)) {
		for _, rule := range askRules {
			for _, re := range rule.pats() {
				if re.MatchString(seg) {
					return core.Ask, warn + core.SafetyMessage(in, core.Ask, rule.what, re.String(), rule.hint), re.String()
				}
			}
		}
	}
	if branch := bareForcePush(cmd, in.CWD); branch != "" {
		return core.Ask, warn + core.SafetyMessage(in, core.Ask, forcePushWhat, "", forcePushHint), "force push of " + branch
	}
	d, msg, detail = askPatternVerdict(in, cmd, scan)
	return d, warn + msg, detail
}

// askPatternVerdict asks on hard rule 4 git verbs, hard rule 3 footers and destructive SQL.
func askPatternVerdict(in *core.Input, cmd, scan string) (core.Decision, string, string) {
	if verb := harnessGitVerb(cmd, in.CWD); verb != "" {
		what := "git stash/checkout/switch/reset/restore/clean inside the shared config tree (CLAUDE.md hard rule 4)"
		if verb == shell.Uninspectable {
			what = "command nests too deep to check, inside the shared config tree (CLAUDE.md hard rule 4)"
		}
		hint := "Scope a build or diff your own files instead; commit early. Use a linked worktree instead: claude-code worktree create <name>."
		return core.Ask, core.SafetyMessage(in, core.Ask, what, "", hint), "shared config git mutation"
	}
	if shell.AttributionFooterCommit(cmd) {
		what := "AI-attribution footer in a commit message (CLAUDE.md hard rule 3)"
		hint := "Rerun this commit without the Co-Authored-By / Generated-with lines; that needs no approval."
		return core.Ask, core.SafetyMessage(in, core.Ask, what, "", hint), "attribution footer"
	}
	if sqlDestructive().MatchString(strings.ToLower(scan)) {
		hint := "Approve only if you asked for it; back up first or run it against a scratch database."
		return core.Ask, core.SafetyMessage(in, core.Ask, "destructive SQL (DROP/TRUNCATE)", "", hint), "destructive SQL"
	}
	return core.Allow, "", ""
}

// bannedGitVerbs are the tree-writing verbs hard rule 4 bans inside the shared config checkout.
var bannedGitVerbs = map[string]bool{"stash": true, "checkout": true, "reset": true, "restore": true, "clean": true, "switch": true}

// harnessGitVerb is the first banned verb (or shell.Uninspectable, nesting too deep to check) whose target tree
// (--work-tree / --git-dir / -C chain, else the segment's cwd after any `cd`) is under the config dir.
func harnessGitVerb(cmd, cwd string) string {
	var harness, worktrees, hit string
	shell.EachGit(dropRemoteHeredocs(dropDataHeredocs(cmd)), cwd, func(target, verb string, args []string) bool {
		if !gitBannedUnder(target, verb, args, &harness, &worktrees) {
			return false
		}
		hit = verb
		return true
	})
	return hit
}

// bannedGit is a hard rule 4 verb that writes the tree; read-only `stash list|show` is fine.
func bannedGit(verb string, args []string) bool {
	readOnly := verb == "stash" && len(args) > 0 && (args[0] == "list" || args[0] == "show")
	return (bannedGitVerbs[verb] || verb == shell.Uninspectable) && !readOnly
}

// gitBannedUnder reports whether one git invocation is a banned verb targeting the harness tree.
func gitBannedUnder(target, verb string, args []string, harness, worktrees *string) bool {
	if !bannedGit(verb, args) {
		return false
	}
	if *harness == "" {
		*harness = foldCase(core.CanonPath(paths.ConfigDir()))
		*worktrees = foldCase(core.CanonPath(filepath.Join(paths.ConfigDir(), ".claude", "worktrees")))
	}
	tp := foldCase(core.CanonPath(target))
	if !paths.Within(tp, *harness) {
		return false
	}
	return !strings.HasPrefix(tp, *worktrees+string(filepath.Separator)) // a linked worktree is its own checkout
}

// foldCase lowercases p where the filesystem is case-insensitive by default (macOS, Windows), so `.CLAUDE` matches.
func foldCase(p string) string {
	if runtime.GOOS == "linux" {
		return p
	}
	return strings.ToLower(p)
}

// hardBlockVerdict blocks a hard shape outside inert text, asks on one only in quoted text; `<<` keeps heredoc bodies.
func hardBlockVerdict(in *core.Input, cmd, scan string) (core.Decision, string, string) {
	if strings.Contains(cmd, "<<") {
		scan = cmd
	}
	if pattern, ok := firstHardBlock(scan); ok {
		return core.Block, core.SafetyMessage(in, core.Block, "destructive command", pattern, terminalHint), pattern
	}
	if scan == cmd {
		return core.Allow, "", "" // nothing was stripped, so the raw pass would find nothing new
	}
	if pattern, ok := firstHardBlock(cmd); ok {
		what := "destructive command (matched only inside quoted/inert text)"
		return core.Ask, core.SafetyMessage(in, core.Ask, what, pattern, terminalHint), pattern
	}
	return core.Allow, "", ""
}

// firstHardBlock is the first hard-block pattern any shell segment of text matches, as written or as the shell reads it
func firstHardBlock(text string) (string, bool) {
	for _, seg := range shell.Segments(text) {
		read := shellRead(seg)
		for _, re := range hardBlocks() {
			if re.MatchString(seg) || re.MatchString(read) {
				return re.String(), true
			}
		}
	}
	return "", false
}

// shellRead is seg with its words unquoted and unescaped and each path operand cleaned: `"rm" -rf //` reads `rm -rf /`.
func shellRead(seg string) string {
	toks := shell.Tokens(seg)
	for i, t := range toks {
		toks[i] = cleanOperand(t)
	}
	return strings.Join(toks, " ")
}

// cleanOperand collapses `//`, `.` and `..` in an absolute or home-relative word; a home path climbing out reads `/`
func cleanOperand(word string) string {
	for _, home := range []string{"${HOME}", "$HOME", "~"} {
		rest, ok := strings.CutPrefix(word, home)
		if !ok || (rest != "" && rest[0] != '/') {
			continue
		}
		switch up := path.Clean("h" + rest); {
		case up == "h":
			return "$HOME"
		case strings.HasPrefix(up, "h/"):
			return "$HOME" + up[1:]
		default:
			return "/"
		}
	}
	if strings.HasPrefix(word, "/") {
		return path.Clean(word)
	}
	return word
}

// bareForcePush is the protected branch a force-push names no refspec for (it updates the checked-out one), else "".
func bareForcePush(cmd, cwd string) string {
	if !strings.Contains(cmd, "push") {
		return ""
	}
	var branch string
	shell.EachGit(dropRemoteHeredocs(cmd), cwd, func(target, verb string, args []string) bool {
		if cwd == "" && !filepath.IsAbs(target) { // a relative target needs a cwd to resolve
			return false
		}
		if verb == "push" && forcesPush(args) && !slices.Contains(args, "--tags") && impliedRefspec(args) {
			branch = protectedHead(target)
		}
		return branch != ""
	})
	return branch
}

// forcesPush reports --force, --force-with-lease[=…] or a short-flag cluster containing f among push args.
func forcesPush(args []string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return a == "--force" || strings.HasPrefix(a, "--force-with-lease") ||
			(strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "f"))
	})
}

// pushValueFlags take their value as the next word, which is neither the remote nor a refspec.
var pushValueFlags = []string{"-o", "--push-option", "--repo", "--receive-pack", "--exec"}

// impliedRefspec reports a push whose words are at most a remote and HEAD, so it updates the checked-out branch.
func impliedRefspec(args []string) bool {
	var words []string
	for i := 0; i < len(args); i++ {
		switch {
		case slices.Contains(pushValueFlags, args[i]):
			i++
		case !strings.HasPrefix(args[i], "-"):
			words = append(words, args[i])
		}
	}
	return len(words) <= 1 || (len(words) == 2 && words[1] == "HEAD")
}

// protectedHead is the branch checked out in dir when it is main or master, else "" (detached, no repo);
// a git that cannot run or times out leaves the branch unknown, reported as protected so the push asks.
func protectedHead(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), rewriteTimeout)
	defer cancel()
	lines, ok, err := core.Git(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return "an unknown branch (git failed)"
	}
	if !ok || len(lines) != 1 || (lines[0] != "main" && lines[0] != "master") {
		return ""
	}
	return lines[0]
}
