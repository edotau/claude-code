// Package safety is the command-safety hook area: safety-scan, and the rtk rewrite.
package safety

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

// rmRecursive matches `rm` with any recursive flag spelling (-r, -Rf, -r -f, --recursive) and trailing operands.
const rmRecursive = `rm\s+(-\S+\s+)*(-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\s+(\S+\s+)*`

// hardBlocks are command shapes that are almost never intentional.
var hardBlocks = core.LazyCompile(
	rmRecursive+`["']?/(\*|\s|$|[)\x60"'])`,
	rmRecursive+`["']?/(etc|usr|var|bin|sbin|lib|System|Users|home)(/?\s|/?$|/?[)\x60"'])`,
	rmRecursive+`["']?(\$HOME|\$\{HOME\}|~)/?(\*|\s|$|[)\x60"'])`,
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
	d, msg, detail, _ := hardBlockVerdict(in, cmd, scan)
	if d == core.Block {
		return d, msg, detail
	}
	for _, re := range exfilPatterns() {
		if re.MatchString(scan) {
			return core.Block, core.SafetyMessage(in, core.Block, "looks like credential exfiltration", re.String(), terminalHint), re.String()
		}
	}
	ask, askMsg, askDetail := safetyAskVerdict(in)
	if d > ask {
		return d, askMsg + msg, detail
	}
	return ask, askMsg, askDetail
}

// safetyAskVerdict reports the first intent-dependent shape; the owner approves it on Claude, others block.
func safetyAskVerdict(in *core.Input) (core.Decision, string, string) {
	cmd := in.ToolCommand()
	// The rewrite block runs first: an ask shape (`sudo git reset --hard`) must not turn a block into an ask.
	d, msg, detail, warn := rewriteVerdict(in)
	if d != core.Allow {
		return d, warn + msg, detail
	}
	// Each pattern is matched inside ONE shell segment: `git push origin main; readlink -f x` is not a force-push.
	for _, seg := range shell.Segments(cmd) {
		for _, rule := range askRules {
			for _, re := range rule.pats() {
				if re.MatchString(seg) {
					return core.Ask, warn + core.SafetyMessage(in, core.Ask, rule.what, re.String(), rule.hint), re.String()
				}
			}
		}
	}
	d, msg, detail = askPatternVerdict(in, cmd)
	return d, warn + msg, detail
}

// askPatternVerdict asks on hard rule 4 git verbs, hard rule 3 footers and destructive SQL.
func askPatternVerdict(in *core.Input, cmd string) (core.Decision, string, string) {
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
	if sqlDestructive().MatchString(strings.ToLower(inertStripped(cmd))) {
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
		*harness = core.CanonPath(paths.ConfigDir())
		*worktrees = core.CanonPath(filepath.Join(paths.ConfigDir(), ".claude", "worktrees"))
	}
	tp := core.CanonPath(target)
	if tp != *harness && !strings.HasPrefix(tp, *harness+string(filepath.Separator)) {
		return false
	}
	return !strings.HasPrefix(tp, *worktrees+string(filepath.Separator)) // a linked worktree is its own checkout
}

// dropRemoteHeredocs removes quoted-delimiter heredoc bodies fed to ssh: they run remotely, unexpanded locally.
func dropRemoteHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		m := heredocOp.FindStringSubmatch(lines[i])
		if m == nil || m[4] != "" || !sshHoldsHeredoc(lines[i]) { // m[4]: bare delimiter, expanded locally
			continue
		}
		if end := heredocEnd(lines, i+1, m[2]+m[3]+m[4], m[1] == "-"); end >= 0 {
			i = end - 1
		}
	}
	return strings.Join(out, "\n")
}

// sshHoldsHeredoc reports an ssh segment carrying the heredoc operator whose output is not piped on.
func sshHoldsHeredoc(line string) bool {
	segs, seps := shell.SegmentsSep(line)
	for i := 0; i < len(segs); i++ {
		seg, sep := segs[i], seps[i]
		for i+1 < len(segs) && sep == "&" && redirectJoin(seg, segs[i+1]) { // `2>&1` / `&>f` are not separators
			i++
			seg, sep = seg+"&"+segs[i], seps[i]
		}
		if heredocOp.MatchString(seg) {
			toks := shell.Tokens(seg)
			return len(toks) > 0 && toks[0] == "ssh" && sep != "|" && sep != "|&"
		}
	}
	return false
}

// redirectJoin reports that the `&` between left and right belongs to a redirection (`>&`, `&>`).
func redirectJoin(left, right string) bool {
	return strings.HasSuffix(strings.TrimSpace(left), ">") || strings.HasPrefix(right, ">")
}

// heredocOp matches one `<<[-]DELIM` operator; groups: dash, single-quoted, double-quoted, bare delimiter.
var heredocOp = regexp.MustCompile(`<<(-?)\s*(?:'([^']*)'|"([^"]*)"|([A-Za-z_][A-Za-z0-9_]*))`)

// fdToSubst matches a redirect to fd 3+, which `exec 3> >(sh)` can point at an interpreter.
var fdToSubst = regexp.MustCompile(`[<>]&[3-9]`)

// inertStripped blanks text that is only data (message values, echo/grep arguments, file-only heredoc bodies)
// but keeps every command substitution; pipeBlocks, exfilPatterns and sqlDestructive match this view.
func inertStripped(cmd string) string {
	if strings.Contains(cmd, ">(") || strings.Contains(cmd, "<(") || fdToSubst.MatchString(cmd) {
		return cmd
	}
	segs, seps := shell.SegmentsSep(dropInertHeredocs(cmd))
	var b strings.Builder
	for i, seg := range segs {
		// A piped segment feeds the next command, so its text stays: `echo "curl x" | sh` runs curl.
		if inertSegment(seg) && (seps[i] == "" || seps[i][0] != '|' || seps[i] == "||") {
			_, subs := shell.ExtractSubstitutions(seg)
			seg = ""
			var kept strings.Builder
			for _, sub := range subs {
				kept.WriteString(" $(")
				kept.WriteString(sub)
				kept.WriteString(")")
			}
			seg += kept.String()
		}
		b.WriteString(seg)
		b.WriteString(seps[i])
	}
	return b.String()
}

// inertSegment reports a segment whose words are data: echo/printf, grep/rg patterns, commit/tag/gh text.
func inertSegment(seg string) bool {
	toks := shell.Tokens(seg)
	if len(toks) == 0 {
		return false
	}
	inert, ok := inertCommands[toks[0]]
	return ok && inert(toks)
}

// inertCommands judge, per command name, whether a segment's words are only data.
var inertCommands = map[string]func(toks []string) bool{
	"echo": inertEcho, "printf": inertEcho, "grep": inertGrep, "rg": inertGrep, "git": inertGit, "gh": inertGh,
	"claude-code": inertScm,
}

// inertEcho: a redirect can plant the text in a file that later runs (~/.bashrc).
func inertEcho(toks []string) bool {
	return !slices.ContainsFunc(toks, func(t string) bool { return strings.Contains(t, ">") })
}

func inertGrep(toks []string) bool {
	return !slices.ContainsFunc(toks, func(t string) bool {
		return strings.HasPrefix(t, "--pre") || strings.HasPrefix(t, "--hostname-bin")
	})
}

func inertGit(toks []string) bool {
	return len(toks) > 1 && (toks[1] == "commit" || toks[1] == "tag")
}

func inertGh(toks []string) bool {
	return len(toks) > 3 && (toks[1] == "pr" || toks[1] == "issue") && slices.Contains([]string{"create", "edit", "comment"}, toks[2])
}

// inertScm: the --title of `claude-code scm pr create|comment` is data; a --body-file path or a redirect is not.
func inertScm(toks []string) bool {
	if len(toks) < 4 || toks[1] != "scm" || !inertEcho(toks) ||
		slices.ContainsFunc(toks, func(t string) bool { return strings.HasPrefix(t, "--body-file") }) {
		return false
	}
	return (toks[2] == "pr" && (toks[3] == "create" || toks[3] == "comment")) || toks[2] == "comment"
}

// dropInertHeredocs removes file-only heredoc bodies unless the command also runs a file.
func dropInertHeredocs(cmd string) string {
	lines := strings.Split(cmd, "\n")
	if runsWrittenFile.MatchString(lines[0]) || heredocTail(lines) {
		return cmd
	}
	var out []string
	for i := 0; i < len(lines); i++ {
		out = append(out, lines[i])
		delim, quoted, dash, ok := inertHeredocStart(lines[i])
		if !ok {
			continue
		}
		end := heredocEnd(lines, i+1, delim, dash)
		// An unquoted delimiter expands substitutions in the body, so a body holding one is not inert.
		body := strings.Join(lines[i+1:max(end, i+1)], "\n")
		if end < 0 || !quoted && expandsSubstitution(body) {
			continue
		}
		i = end - 1
	}
	return strings.Join(out, "\n")
}

// heredocEnd is the index of the first line from lines[from] that closes the heredoc on delim, or -1.
func heredocEnd(lines []string, from int, delim string, dash bool) int {
	for j := from; j < len(lines); j++ {
		if l := lines[j]; l == delim || dash && strings.TrimLeft(l, "\t") == delim {
			return j
		}
	}
	return -1
}

// expandsSubstitution reports a heredoc body holding a `$(…)` or backtick substitution.
func expandsSubstitution(body string) bool {
	return strings.Contains(body, "$(") || strings.Contains(body, "`")
}

// inertHeredocStart parses a lone `cat|tee … <<DELIM` line that only writes a file; anything fancier is not inert.
func inertHeredocStart(line string) (delim string, quoted, dash, ok bool) {
	if strings.ContainsAny(line, "#|`&(;") || strings.Contains(line, "<<<") || strings.HasSuffix(line, `\`) {
		return
	}
	ms := heredocOp.FindAllStringSubmatch(line, -1)
	if len(ms) != 1 {
		return
	}
	toks := shell.Tokens(heredocOp.ReplaceAllString(line, " "))
	if len(toks) == 0 || !strings.HasPrefix(strings.TrimLeft(line, " \t"), toks[0]+" ") || !writesFile(toks) {
		return
	}
	m := ms[0]
	delim = m[2] + m[3] + m[4]
	return delim, m[4] == "", m[1] == "-", delim != ""
}

// writesFile reports a `cat` with a `> file` redirect or a `tee` with a file operand.
func writesFile(toks []string) bool {
	switch toks[0] {
	case "tee":
		return slices.ContainsFunc(toks[1:], func(t string) bool {
			return t != "" && t[0] != '-' && !strings.ContainsAny(t, "<>")
		})
	case "cat":
		return catRedirects(toks)
	}
	return false
}

// catRedirects reports a `>`/`>>` token followed by a file, or a `>file` token.
func catRedirects(toks []string) bool {
	for i, t := range toks {
		if (t == ">" || t == ">>") && i+1 < len(toks) || len(t) > 1 && t[0] == '>' {
			return true
		}
	}
	return false
}

// hardBlockVerdict blocks a hard shape outside inert text, asks on one only in quoted text; `<<` keeps heredoc bodies.
func hardBlockVerdict(in *core.Input, cmd, scan string) (core.Decision, string, string, bool) {
	if strings.Contains(cmd, "<<") {
		scan = cmd
	}
	if pattern, ok := firstHardBlock(scan); ok {
		return core.Block, core.SafetyMessage(in, core.Block, "destructive command", pattern, terminalHint), pattern, true
	}
	if scan == cmd {
		return core.Allow, "", "", false // nothing was stripped, so the raw pass would find nothing new
	}
	if pattern, ok := firstHardBlock(cmd); ok {
		what := "destructive command (matched only inside quoted/inert text)"
		return core.Ask, core.SafetyMessage(in, core.Ask, what, pattern, terminalHint), pattern, true
	}
	return core.Allow, "", "", false
}

// firstHardBlock is the first hard-block pattern any shell segment of text matches.
func firstHardBlock(text string) (string, bool) {
	for _, seg := range shell.Segments(text) {
		for _, re := range hardBlocks() {
			if re.MatchString(seg) {
				return re.String(), true
			}
		}
	}
	return "", false
}
