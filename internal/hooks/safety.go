package hooks

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

// rmRecursive matches `rm` with any recursive flag spelling (-r, -Rf, -r -f, --recursive) and trailing flags.
const rmRecursive = `rm\s+(-\S+\s+)*(-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\s+(-\S+\s+)*`

// hardBlocks are command shapes that are almost never intentional.
var hardBlocks = core.LazyCompile(
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
var pipeBlocks = core.LazyCompile(
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

// forceFlag is a standalone --force or short-flag cluster containing f (-f, -fu, -uf).
const forceFlag = `(--force|-[a-zA-Z]*f[a-zA-Z]*)(\s|$)`

// protectedBranch is main/master as a whole refspec word: after whitespace, a quote, `:` or refs/heads/ (not feature-main).
const protectedBranch = `(.*[\s"':]|.*refs/heads/)?(main|master)["']?(\s|$)`

var askRules = []askRule{
	{"sudo command", "Approve only if you asked for it; otherwise install to a user prefix (~/.local, pipx, a venv) instead.",
		core.LazyCompile(`(^|[^[:alnum:]_])sudo(\s|$)`)},
	{"force push to main/master", "Push a branch and open a PR instead: git push origin HEAD:<branch>.", core.LazyCompile(
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
	return regexp.MustCompile(`(^|[^[:alnum:]_])(drop\s+database|drop\s+table|truncate\s+table)([^[:alnum:]_]|$)`)
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
			return core.Block, core.SafetyMessage(in, core.Block, "destructive command", re.String(), terminalHint), re.String()
		}
	}
	if d, msg, detail, ok := hardBlockVerdict(in, cmd, scan); ok {
		return d, msg, detail
	}
	for _, re := range exfilPatterns() {
		if re.MatchString(scan) {
			return core.Block, core.SafetyMessage(in, core.Block, "looks like credential exfiltration", re.String(), terminalHint), re.String()
		}
	}
	return safetyAskVerdict(in)
}

// safetyAskVerdict reports the first intent-dependent shape; the owner approves it on Claude, others block.
func safetyAskVerdict(in *core.Input) (core.Decision, string, string) {
	cmd := in.ToolCommand()
	// Each pattern is matched inside ONE shell segment: `git push origin main; readlink -f x` is not a force-push.
	for _, seg := range shell.Segments(cmd) {
		for _, rule := range askRules {
			for _, re := range rule.pats() {
				if re.MatchString(seg) {
					return core.Ask, core.SafetyMessage(in, core.Ask, rule.what, re.String(), rule.hint), re.String()
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
		return core.Ask, core.SafetyMessage(in, core.Ask, what, "", hint), "shared config git mutation"
	}
	if attributionFooterBlocked(cmd) {
		what := "AI-attribution footer in a commit message (CLAUDE.md hard rule 3)"
		hint := "Rerun this commit without the Co-Authored-By / Generated-with lines; that needs no approval."
		return core.Ask, core.SafetyMessage(in, core.Ask, what, "", hint), "attribution footer"
	}
	if sqlDestructive().MatchString(strings.ToLower(inertStripped(cmd))) {
		hint := "Approve only if you asked for it; back up first or run it against a scratch database."
		return core.Ask, core.SafetyMessage(in, core.Ask, "destructive SQL (DROP/TRUNCATE)", "", hint), "destructive SQL"
	}
	return core.Allow, "", ""
}

// bannedGitVerbs are the tree-writing verbs hard rule 4 bans inside the shared config checkout.
var bannedGitVerbs = map[string]bool{"stash": true, "checkout": true, "reset": true, "restore": true, "clean": true, "switch": true}

// harnessGitBlocked reports a banned git verb whose target tree (--work-tree / --git-dir / -C chain, else the
// segment's cwd after any `cd`) sits under the config dir; shell segments and global options are parsed, not regexed.
func harnessGitBlocked(cmd, cwd string) bool {
	var harness, worktrees string
	return shell.EachGit(dropRemoteHeredocs(cmd), cwd, func(target, verb string, args []string) bool {
		return gitBannedUnder(target, verb, args, &harness, &worktrees)
	})
}

// bannedGit is a hard rule 4 verb that writes the tree; read-only `stash list|show` is fine.
func bannedGit(verb string, args []string) bool {
	readOnly := verb == "stash" && len(args) > 0 && (args[0] == "list" || args[0] == "show")
	return bannedGitVerbs[verb] && !readOnly
}

// gitBannedUnder reports whether one git invocation is a banned verb targeting the harness tree.
func gitBannedUnder(target, verb string, args []string, harness, worktrees *string) bool {
	if !bannedGit(verb, args) {
		return false
	}
	if *harness == "" {
		*harness = core.CanonPath(paths.ConfigDir())
		*worktrees = core.CanonPath(filepath.Join(paths.ConfigDir(), ".claude", "worktrees"))
	}
	tp := core.CanonPath(target)
	if tp != *harness && !strings.HasPrefix(tp, *harness+string(filepath.Separator)) {
		return false
	}
	return !strings.HasPrefix(tp, *worktrees+string(filepath.Separator)) // a linked worktree is its own checkout
}

// dropRemoteHeredocs removes heredoc bodies fed to ssh: they run on the remote host, not in this checkout.
func dropRemoteHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		toks := shell.Tokens(lines[i])
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
