// Package safety is the command-safety hook area: safety-scan, permission-gate and the rtk rewrite.
package safety

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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

// askRule is an intent-dependent shape matched within one shell segment, its message, alternative and exemption.
type askRule struct {
	what, hint string
	pats       func() []*regexp.Regexp
	exempt     func(seg, cmd, cwd string) bool
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
		core.LazyCompile(`(^|[^[:alnum:]_])sudo(\s|$)`), nil},
	{forcePushWhat, forcePushHint, core.LazyCompile(
		gitPushPrefix+`(.*\s)?`+forceFlag+protectedBranch,
		gitPushPrefix+protectedBranch+`(.*\s)?`+forceFlag,
		gitPushPrefix+`.*--force-with-lease(=[^[:space:]]*)?(\s|$)`+protectedBranch,
		gitPushPrefix+protectedBranch+`.*--force-with-lease(=[^[:space:]]*)?(\s|$)`,
		gitPushPrefix+`.*\+([^[:space:]]+:)?(refs/heads/)?(main|master)["']?(\s|$)`,
	), nil},
	{"recursive removal of a .git directory",
		"That deletes the repository history; to start clean use a fresh worktree: claude-code worktree create <name>.",
		core.LazyCompile(`(^|[^[:alnum:]_])rm\s+(-\S+\s+)*(-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\s+(([^[:space:]"']+|"[^"]*"|'[^']*')\s+)*["']?([^[:space:]"']*/)?\.git/?["']?(\s|$)`),
		tempGitRemoval},
}

const terminalHint = "If intentional, run it yourself in a terminal."

// sqlDestructive runs against the lowercased command.
var sqlDestructive = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|[^[:alnum:]_])(drop\s+(database|table|schema)|truncate\s+table|truncate\s+[a-z_"][a-z0-9_."]*\s*(;|$|cascade|["']))([^[:alnum:]_]|$)`)
})

// credRead is a reader of a credential file, the left side of exfilPatterns[1].
const credRead = `(cat|tail|head|base64)\s+.*(\.claude/settings\.json|\.credentials\.json|secrets\.env|\.ssh/|\.aws/credentials|\.netrc)`

// exfilPatterns match credential-exfiltration shapes.
var exfilPatterns = core.LazyCompile(
	`(^|[\s;&|({/\x60'"\\])(env|printenv)['"]?\s*>\s*/tmp/`,
	credRead+`.*\|.*(^|[^[:alnum:]_])(curl|nc|ncat|wget)([^[:alnum:]_]|$)`,
	`base64\s+.*(\.ssh/id_|\.aws/credentials|\.credentials\.json|secrets\.env)`,
	`(scp|rsync)\s+.*\.ssh/id_`,
)

// toolEnv is a tool's own `env` subcommand (`go env`): it prints the tool's settings, not the environment.
var toolEnv = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|[\s;&|({\x60])(\S*/)?(go|conda|mamba|poetry)[ \t]+env([ \t]*>)`)
})

var credReader = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(credRead) })

// exfilMatch is exfilPatterns[i] on scan, minus a tool's `env` subcommand and credentials cmd only prints.
func exfilMatch(i int, re *regexp.Regexp, scan, cmd string) bool {
	switch i {
	case 0:
		scan = toolEnv().ReplaceAllString(scan, "${1}${3}${4}")
	case 1:
		if credsShownOnly(cmd) {
			return false
		}
	}
	return re.MatchString(scan)
}

// credsShownOnly reports every credential read is a cat/head/tail piped only to filters, with nothing to carry it on.
func credsShownOnly(cmd string) bool {
	if strings.ContainsAny(cmd, "(){}<>`$") {
		return false
	}
	segs, seps := shell.SegmentsSep(cmd)
	for k, seg := range segs {
		if !credReader().MatchString(seg) {
			continue
		}
		toks := shell.Tokens(seg)
		piped := seps[k] == "|" || seps[k] == "|&"
		if k > 0 && (seps[k-1] == "|" || seps[k-1] == "|&") || len(toks) == 0 ||
			!slices.Contains([]string{"cat", "head", "tail"}, toks[0]) || piped && !filtersOnly(segs, seps, k+1) {
			return false
		}
	}
	return true
}

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
	if decision != core.Block && (in.ToolName == "" || strings.EqualFold(in.CanonicalTool(), "Bash")) {
		cmd := blankInert(dropMessageHeredocs(dropPatchBodies(in.ToolCommand()))) // a message only names the shape
		for _, re := range softWarn() {
			if re.MatchString(cmd) {
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
	audit.LogSession(audit.HookBlocked, in.SessionID, "safety", detail)
	if decision == core.Ask && in.Agent() == core.AgentClaude {
		return core.Result{Decision: core.Ask, Reason: strings.TrimSpace(msg), Stderr: strings.TrimSpace(warn.String())}
	}
	return core.Result{Decision: core.Block, Reason: strings.TrimSpace(warn.String() + msg)}
}

// Verdict is safety-scan's decision for in with no audit entry or warning, for the doctor permissions report.
func Verdict(in *core.Input) core.Decision { d, _, _ := safetyVerdict(in); return d }

// safetyVerdict checks all block patterns before considering any ask pattern.
func safetyVerdict(in *core.Input) (core.Decision, string, string) {
	if in.ToolName != "" && !strings.EqualFold(in.CanonicalTool(), "Bash") {
		return core.Allow, "", ""
	}
	cmd := dropMessageHeredocs(dropPatchBodies(in.ToolCommand()))
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
	for i, re := range exfilPatterns() {
		if exfilMatch(i, re, scan, cmd) {
			return core.Block, core.SafetyMessage(in, core.Block, "looks like credential exfiltration", re.String(), terminalHint), re.String()
		}
	}
	ask, askMsg, askDetail := safetyAskVerdict(in, cmd, scan)
	if d > ask {
		return d, askMsg + msg, detail
	}
	return ask, askMsg, askDetail
}

// safetyAskVerdict reports the first intent-dependent shape; the owner approves it on Claude, others block.
func safetyAskVerdict(in *core.Input, cmd, scan string) (core.Decision, string, string) {
	// The rewrite block runs first: an ask shape (`sudo git reset --hard`) must not turn a block into an ask.
	d, msg, detail, warn := rewriteVerdict(in, cmd)
	if d != core.Allow {
		return d, warn + msg, detail
	}
	// Rules match inside ONE shell segment of inert-blanked text: `git push origin main; readlink -f x` is no force-push.
	for _, seg := range shell.Segments(blankInert(cmd)) {
		for _, rule := range askRules {
			for _, re := range rule.pats() {
				if re.MatchString(seg) && (rule.exempt == nil || !rule.exempt(seg, cmd, in.CWD)) {
					return core.Ask, warn + core.SafetyMessage(in, core.Ask, rule.what, re.String(), rule.hint), re.String()
				}
			}
		}
	}
	if d, msg, detail = pushOrScriptVerdict(in, cmd); d != core.Allow {
		return d, warn + msg, detail
	}
	d, msg, detail = askPatternVerdict(in, cmd, scan)
	return d, warn + msg, detail
}

// pushOrScriptVerdict asks on a force-push with no refspec and on an interpreter that may write a protected file.
func pushOrScriptVerdict(in *core.Input, cmd string) (core.Decision, string, string) {
	if branch := bareForcePush(cmd, in.CWD); branch != "" {
		return core.Ask, core.SafetyMessage(in, core.Ask, forcePushWhat, "", forcePushHint), "force push of " + branch
	}
	if p := interpreterWrite(cmd); p != "" {
		hint := "Approve only if you asked for it; otherwise edit it by hand or run it yourself in a terminal."
		what := "an interpreter script that may write a file Edit/Write may never touch"
		return core.Ask, core.SafetyMessage(in, core.Ask, what, p, hint), "interpreter protected write"
	}
	return core.Allow, "", ""
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
	if shell.AttributionFooterCommit(dropPatchBodies(in.ToolCommand())) { // a footer lives in the message body
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
	cmd = dropRemoteHeredocs(core.DropDataHeredocs(cmd))
	hit := bannedVerb(cmd, cwd)
	if hit != shell.Uninspectable {
		return hit
	}
	// A printf/echo fed to a shell that decodes to text running no git is not too deep to check.
	outer, texts := shellFeeds(cmd)
	if len(texts) == 0 || bannedVerb(outer, cwd) != "" || slices.ContainsFunc(texts, runsGit) {
		return hit
	}
	return ""
}

// bannedVerb is harnessGitVerb's first hit in cmd, with no feed decoding.
func bannedVerb(cmd, cwd string) string {
	var hit string
	shell.EachGit(cmd, cwd, func(target, verb string, args []string) bool {
		if !gitBannedUnder(target, verb, args) {
			return false
		}
		hit = verb
		return true
	})
	return hit
}

// runsGit reports text holding any git invocation or nesting EachGit cannot see into, wherever it would run.
func runsGit(text string) bool {
	return shell.EachGit(text, "/", func(string, string, []string) bool { return true })
}

// bannedGit is a hard rule 4 verb that writes the tree; read-only `stash list|show` and `clean -n` are fine.
func bannedGit(verb string, args []string) bool {
	readOnly := verb == "stash" && len(args) > 0 && (args[0] == "list" || args[0] == "show") ||
		verb == "clean" && cleanDryRun(args)
	return (bannedGitVerbs[verb] || verb == shell.Uninspectable) && !readOnly
}

// cleanDryRun reports a git clean given -n/--dry-run, which removes nothing, and not -i, which prompts to delete.
func cleanDryRun(args []string) bool {
	dry := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return dry
		case a == "--dry-run":
			dry = true
		case a == "--interactive":
			return false
		case a == "--exclude":
			i++ // the exclude pattern
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
			flags, pattern, hasE := strings.Cut(a[1:], "e") // -e takes the rest of the cluster, else the next word
			if hasE && pattern == "" {
				i++
			}
			if strings.Contains(flags, "i") {
				return false
			}
			dry = dry || strings.Contains(flags, "n")
		}
	}
	return dry
}

// gitBannedUnder reports whether one git invocation is a banned verb targeting the harness tree.
func gitBannedUnder(target, verb string, args []string) bool {
	if !bannedGit(verb, args) {
		return false
	}
	worktrees := filepath.Join(paths.ConfigDir(), ".claude", "worktrees") // a linked worktree is its own checkout
	return paths.InsideDir(target, paths.ConfigDir()) &&
		(!paths.InsideDir(target, worktrees) || paths.SameFile(target, worktrees))
}

// hardBlockVerdict blocks a protected write or a hard shape outside inert text and asks on one only in quoted text.
func hardBlockVerdict(in *core.Input, cmd, scan string) (core.Decision, string, string) {
	if p := protectedWrite(cmd, in.CWD); p != "" {
		return core.Block, core.SafetyMessage(in, core.Block, "writes a file Edit/Write may never touch", p,
			"Edit it by hand, or run it yourself in a terminal."), "protected write"
	}
	if strings.Contains(cmd, "<<") { // heredoc bodies stay in the scan
		scan = cmd
	}
	if pattern, ok := firstHardBlock(scan); ok {
		return core.Block, core.SafetyMessage(in, core.Block, "destructive command", pattern, terminalHint), pattern
	}
	if pattern, ok := feedHardBlock(cmd); ok { // `printf 'rm -rf /\n' | sh` runs what printf prints
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

// feedHardBlock is the first hard-block pattern the text a printf/echo pipes into a shell matches, decoded.
func feedHardBlock(cmd string) (string, bool) {
	_, texts := shellFeeds(cmd)
	for _, text := range texts {
		if pattern, ok := firstHardBlock(text); ok {
			return pattern, true
		}
	}
	return "", false
}

// shellFeeds is cmd with each decodable printf/echo-into-a-shell pipeline replaced by `:`, and the texts it runs.
func shellFeeds(cmd string) (string, []string) {
	if !strings.Contains(cmd, "|") {
		return cmd, nil
	}
	segs, seps := shell.SegmentsSep(cmd)
	var b strings.Builder
	var texts []string
	for i := 0; i < len(segs); i++ {
		if end, text, ok := shellFeed(segs, seps, i); ok {
			texts = append(texts, text...)
			b.WriteString(" :")
			b.WriteString(seps[end])
			i = end
			continue
		}
		b.WriteString(segs[i])
		b.WriteString(seps[i])
	}
	return b.String(), texts
}

// shellFeed decodes a printf/echo at segs[i] piped, through filters only, into a stdin shell ending at segs[end].
func shellFeed(segs, seps []string, i int) (end int, texts []string, ok bool) {
	if strings.ContainsAny(segs[i], "$`<>") { // an expansion or redirect cannot be decoded from the text
		return 0, nil, false
	}
	texts, ok = printedText(shell.Tokens(segs[i]))
	for k := i; ok && (seps[k] == "|" || seps[k] == "|&") && k+1 < len(segs); k++ {
		rest, _ := shell.Unwrap(shell.Tokens(segs[k+1]), "")
		switch {
		case stdinShell(rest):
			return k + 1, texts, true
		case len(rest) == 0 || !pipeFilters[rest[0]] && rest[0] != "cat" || !inertEcho(rest):
			return 0, nil, false
		}
	}
	return 0, nil, false
}

// stdinShell reports sh/bash/zsh/dash/ksh given only flags and no -c string: it runs the commands on its stdin.
func stdinShell(toks []string) bool {
	if len(toks) == 0 || !slices.Contains([]string{"sh", "bash", "zsh", "dash", "ksh"}, filepath.Base(toks[0])) {
		return false
	}
	return !slices.ContainsFunc(toks[1:], func(t string) bool {
		return !strings.HasPrefix(t, "-") || !strings.HasPrefix(t, "--") && strings.Contains(t, "c")
	})
}

// printedText is what a printf or echo prints; echo's text is checked as bash and as zsh/dash print it (escapes).
func printedText(toks []string) ([]string, bool) {
	if len(toks) < 2 {
		return nil, false
	}
	words := toks[1:]
	switch toks[0] {
	case "printf":
		if strings.HasPrefix(words[0], "-") { // -v assigns instead of printing; -- is rare
			return nil, false
		}
		return []string{printfOutput(words[0], words[1:])}, true
	case "echo":
		for len(words) > 0 && echoFlag(words[0]) {
			words = words[1:]
		}
		raw := strings.Join(words, " ")
		return []string{raw, decodeEscapes(raw)}, true
	}
	return nil, false
}

// echoFlag reports an echo option word such as -n, -e or -neE.
func echoFlag(w string) bool {
	return len(w) > 1 && w[0] == '-' && strings.Trim(w[1:], "neE") == ""
}

// printfOutput applies a printf format to args as the shell does, reusing the format while args remain.
func printfOutput(format string, args []string) string {
	var out strings.Builder
	for {
		used := printfPass(&out, format, args)
		if args = args[used:]; used == 0 || len(args) == 0 {
			return decodeEscapes(out.String())
		}
	}
}

// printfPass writes one pass of format, `%b` arguments decoded, and returns how many args it consumed.
func printfPass(out *strings.Builder, format string, args []string) int {
	used := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' || i+1 >= len(format) {
			out.WriteByte(format[i])
			continue
		}
		j := i + 1
		for j < len(format) && strings.IndexByte("-+ #0123456789.", format[j]) >= 0 {
			j++
		}
		if j >= len(format) || format[j] == '%' {
			out.WriteByte('%')
			i = j
			continue
		}
		arg := ""
		if used < len(args) {
			arg, used = args[used], used+1
		}
		if format[j] == 'b' {
			arg = decodeEscapes(arg)
		}
		out.WriteString(strings.ReplaceAll(arg, `\`, `\\`)) // literal: survives the final escape pass
		i = j
	}
	return used
}

// decodeEscapes applies printf/echo -e backslash escapes: \n \t \\, octal \0NNN or \NNN, hex \xHH; \c ends output.
func decodeEscapes(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out.WriteByte(s[i])
			continue
		}
		i++
		switch c := s[i]; {
		case c == 'c':
			return out.String()
		case strings.IndexByte("ntrvfab", c) >= 0:
			out.WriteByte("\n\t\r\v\f\a\b"[strings.IndexByte("ntrvfab", c)])
		case c == 'x' || c >= '0' && c <= '7':
			n, width := escapeValue(s[i:])
			out.WriteByte(n)
			i += width - 1
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// escapeValue reads a \xHH (up to 2 hex digits) or \0NNN / \NNN (up to 3 octal digits) escape after its backslash.
func escapeValue(s string) (byte, int) {
	base, digits, from, most := 8, "01234567", 0, 3
	switch {
	case s[0] == 'x':
		base, digits, from, most = 16, "0123456789abcdefABCDEF", 1, 2
	case s[0] == '0' && len(s) > 1 && strings.IndexByte(digits, s[1]) >= 0:
		from = 1
	}
	n, k := 0, from
	for ; k < len(s) && k-from < most && strings.IndexByte(digits, s[k]) >= 0; k++ {
		v, _ := strconv.ParseInt(s[k:k+1], 16, 0)
		n = n*base + int(v)
	}
	if k == from { // `\x` with no hex digit prints as written
		return 'x', 1
	}
	return byte(n), k
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
