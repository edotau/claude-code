// Package safety is the command-safety hook area: safety-scan, permission-gate and the rtk rewrite.
package safety

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/shell"
)

// pipeBlocks match the whole command minus inert data: pipes, a fork bomb and encoded Python heredocs span segments.
var pipeBlocks = []*core.Gated{
	core.Gate(`(^|[^[:alnum:]_])(curl|wget)\s[^|]*\|\s*(sudo\s+)?(\S*/)?(ba|z)?sh([^[:alnum:]_]|$)`, "curl", "wget"),
	core.Gate(`(^|[\s;&|({/\x60])eval\s+["']?(\$\(|\x60)\s*(\S*/)?(curl|wget)\s`, "eval"),
	core.Gate(`(^|[^[:alnum:]_])base64\s+(-d|-D|--decode)[^|]*\|\s*(sudo\s+)?(\S*/)?(ba|z|da|k)?sh([^[:alnum:]_]|$)`, "base64"),
	core.Gate(`:\(\)\s*\{[^}]*\}\s*;\s*:`, ":()"),
	core.Gate(`(^|[\s;&|({/\x60])(bash|sh|zsh|dash|ksh)\s+(-\S*\s+)*-[a-zA-Z]*c[a-zA-Z]*\s+["']?(\$\(\s*|\x60\s*)(\S*/)?(curl|wget)\s`, "curl", "wget"),
	core.Gate(`(^|[\s;&|({/\x60])(bash|sh|zsh|dash|ksh|source|\.)\s+(-\S*\s+)*(<\s*)?<\(\s*(\S*/)?(curl|wget)\s`, "curl", "wget"),
	core.Gate(`(?s)(^|[\s;&|({/\x60])python3?(\.[0-9]+)?\b[^\n]*<<[-<]?.*((exec|eval)\(.*(base64|b64decode)|(base64|b64decode).*(exec|eval)\()`, "python"),
}

// askRule is an intent-dependent shape matched within one shell segment, its message, allowed route and exemption.
type askRule struct {
	what, hint string
	pats       []*core.Gated
	exempt     func(seg, cmd, cwd string) bool
}

const gitPushPrefix = `git\s+(((-[Cc]\s+|--(git-dir|work-tree)(=|\s+))("[^"]*"|'[^']*'|[^\s"']+)|--?[a-zA-Z][a-zA-Z-]*(=\S+)?)\s+)*push\s+`

// forceFlag is a standalone --force or short-flag cluster containing f (-f, -fu, -uf).
const forceFlag = `(--force|-[a-zA-Z]*f[a-zA-Z]*)(\s|$)`

// protectedBranch is main/master as a whole refspec word: after space, a quote, `:` or refs/heads/ (not feature-main)
const protectedBranch = `(.*[\s"':]|.*refs/heads/)?(main|master)["']?(\s|$)`

const (
	forcePushWhat = "force push to main/master"
	forcePushHint = "Push a branch and open a PR instead: git push origin HEAD:<branch>." + ownerRoute
)

var askRules = []askRule{
	{forcePushWhat, forcePushHint, []*core.Gated{
		core.Gate(gitPushPrefix+`(.*\s)?`+forceFlag+protectedBranch, "push"),
		core.Gate(gitPushPrefix+protectedBranch+`(.*\s)?`+forceFlag, "push"),
		core.Gate(gitPushPrefix+`.*--force-with-lease(=[^[:space:]]*)?(\s|$)`+protectedBranch, "push"),
		core.Gate(gitPushPrefix+protectedBranch+`.*--force-with-lease(=[^[:space:]]*)?(\s|$)`, "push"),
		core.Gate(gitPushPrefix+`.*\+([^[:space:]]+:)?(refs/heads/)?(main|master)["']?(\s|$)`, "push"),
	}, nil},
	{"recursive removal of a .git directory",
		"That deletes the repository history; use a fresh worktree instead: claude-code worktree create <name>." + ownerRoute,
		[]*core.Gated{core.Gate(`(^|[^[:alnum:]_])rm\s+(-\S+\s+)*(-[a-zA-Z]*[rR][a-zA-Z]*|--recursive)\s+(([^[:space:]"']+|"[^"]*"|'[^']*')\s+)*["']?([^[:space:]"']*/)?\.git/?["']?(\s|$)`, ".git")},
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
var exfilPatterns = []*core.Gated{
	core.Gate(`(^|[\s;&|({/\x60'"\\])(env|printenv)['"]?\s*>\s*/tmp/`, "/tmp/"),
	core.Gate(credRead+`.*(\|.*(^|[^[:alnum:]_])|>\(([^)]*[^[:alnum:]_])?)(`+strings.Join(networkSinks, "|")+`|openssl\s+s_client)([^[:alnum:]_]|$)`,
		".claude/settings.json", ".credentials.json", "secrets.env", ".ssh/", ".aws/credentials", ".netrc"),
	core.Gate(`base64\s+.*(\.ssh/id_|\.aws/credentials|\.credentials\.json|secrets\.env)`, "base64"),
	core.Gate(`(scp|rsync)\s+.*(\.ssh/id_|\.aws/credentials)`, ".ssh/id_", ".aws/credentials"),
}

// toolEnv is a tool's own `env` subcommand (`go env`): it prints the tool's settings, not the environment.
var toolEnv = core.Gate(`(^|[\s;&|({\x60])(\S*/)?(go|conda|mamba|poetry)[ \t]+env([ \t]*>)`, "env")

var credReader = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(credRead) })

// exfilMatch is exfilPatterns[i] on scan, minus a tool's `env` subcommand, public keys and credentials cmd only prints.
func exfilMatch(i int, re *core.Gated, scan, cmd string) bool {
	if i == 0 && toolEnv.MayMatch(scan) {
		scan = toolEnv.Regexp().ReplaceAllString(scan, "${1}${3}${4}")
	}
	if i == 1 && publicKey.MayMatch(scan) {
		scan = publicKey.Regexp().ReplaceAllString(scan, " ${1}")
	}
	return i == 1 && secretPiped(scan) || re.MatchString(scan) && (i != 1 || !credsShownOnly(cmd))
}

// credsShownOnly reports every credential read is a cat/head/tail piped only to filters, with nothing to carry it on.
func credsShownOnly(cmd string) bool {
	if secretPiped(cmd) || strings.ContainsAny(cmd, "(){}<>`$") {
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
var softWarn = []*core.Gated{
	core.Gate(`kubectl\s+delete(\W|$)`, "kubectl"),
	core.Gate(`docker\s+system\s+prune`, "prune"),
	core.Gate(`git\s+reset\s+--hard`, "--hard"),
	core.Gate(`git\s+clean\s+-[a-z]*f`, "clean"),
}

// Scan checks command safety: a block or non-Claude ask rejects, a Claude ask prompts, soft warnings ride stderr.
func Scan(in *core.Input) core.Result {
	cmd := dropMessageHeredocs(dropPatchBodies(in.ToolCommand()))
	dropped := dropInertHeredocs(cmd)
	scan := blankInert(dropped)
	decision, msg, detail := commandVerdict(in, cmd, scan)
	var warn strings.Builder
	if decision != core.Block && (in.ToolName == "" || strings.EqualFold(in.CanonicalTool(), "Bash")) {
		view := scan // a message only names the shape; with no inert heredoc dropped, scan is cmd's inert view
		if dropped != cmd {
			view = blankInert(cmd)
		}
		for _, re := range softWarn {
			if re.MatchString(view) {
				fmt.Fprintf(&warn, "safety: WARNING — %s (not blocked; review carefully)\n", re)
			}
		}
	}
	if decision == core.Allow { // an allowed verdict's message is a warning (the rewrite guard failing open)
		return core.Result{Stderr: strings.TrimSpace(msg + warn.String())}
	}
	rule := detail // the pattern or bucket the audit records, minus a force push's branch name (command text)
	if strings.HasPrefix(rule, "force push of ") {
		rule = "force push"
	}
	if decision == core.Ask {
		detail = "ask: " + detail
	}
	audit.LogSession(audit.HookBlocked, in.SessionID, "safety", detail)
	if decision == core.Ask && core.SupportsAsk(in.Agent()) {
		return core.Result{Decision: core.Ask, Reason: strings.TrimSpace(msg), Stderr: strings.TrimSpace(warn.String()), Rule: rule}
	}
	return core.Result{Decision: core.Block, Reason: strings.TrimSpace(warn.String() + msg), Rule: rule}
}

// Verdict is safety-scan's decision for in with no audit entry or warning, for the doctor permissions report.
func Verdict(in *core.Input) core.Decision { d, _, _ := safetyVerdict(in); return d }

// safetyVerdict checks all block patterns before considering any ask pattern.
func safetyVerdict(in *core.Input) (core.Decision, string, string) {
	cmd := dropMessageHeredocs(dropPatchBodies(in.ToolCommand()))
	return commandVerdict(in, cmd, inertStripped(cmd))
}

// commandVerdict judges cmd; scan is inertStripped(cmd), computed once by the caller.
func commandVerdict(in *core.Input, cmd, scan string) (core.Decision, string, string) {
	if in.ToolName != "" && !strings.EqualFold(in.CanonicalTool(), "Bash") {
		return core.Allow, "", ""
	}
	if cmd == "" {
		return core.Allow, "", ""
	}
	for _, re := range pipeBlocks {
		if re.MatchString(scan) {
			return core.Block, core.SafetyMessage(in, core.Block, "destructive command", re.String(), terminalHint), re.String()
		}
	}
	if d, msg, detail := hardBlockVerdict(in, cmd, scan); d == core.Block {
		return d, msg, detail
	}
	for i, re := range exfilPatterns {
		if exfilMatch(i, re, scan, cmd) {
			return core.Block, core.SafetyMessage(in, core.Block, "looks like credential exfiltration", re.String(), terminalHint), re.String()
		}
	}
	return safetyAskVerdict(in, cmd)
}

// safetyAskVerdict judges the intent rules on real invocations only: most block, destructive SQL and script writes ask.
func safetyAskVerdict(in *core.Input, cmd string) (core.Decision, string, string) {
	// The rewrite block runs first: an ask shape (`sudo git reset --hard`) must not turn a block into an ask.
	d, msg, detail, warn := rewriteVerdict(in, cmd)
	if d != core.Allow {
		return d, warn + msg, detail
	}
	// A plain Python stdin script's body is Python and data text is no invocation; interpreterWrite judges its paths.
	py := withPyLaunches(dropPyScripts(cmd))
	asked := intentView(py, false)
	if sudoInvoked(asked, in.CWD) {
		return core.Block, warn + core.SafetyMessage(in, core.Block, "sudo command", "", sudoHint), "sudo"
	}
	// Rules match inside ONE shell segment: `git push origin main; readlink -f x` is no force-push.
	for _, seg := range shell.Segments(asked) {
		for _, rule := range askRules {
			for _, re := range rule.pats {
				if re.MatchString(seg) && (rule.exempt == nil || !rule.exempt(seg, asked, in.CWD)) {
					return core.Block, warn + core.SafetyMessage(in, core.Block, rule.what, re.String(), rule.hint), re.String()
				}
			}
		}
	}
	plain := asked // intentView(cmd, false), the same view when no Python script was dropped
	if py != cmd {
		plain = intentView(cmd, false)
	}
	if d, msg, detail = pushOrScriptVerdict(in, plain, asked); d != core.Allow {
		return d, warn + msg, detail
	}
	d, msg, detail = askPatternVerdict(in, asked)
	return d, warn + msg, detail
}

// pushOrScriptVerdict blocks a force-push with no refspec and asks on an interpreter that may write a protected file.
func pushOrScriptVerdict(in *core.Input, plain, asked string) (core.Decision, string, string) {
	if branch := bareForcePush(asked, in.CWD); branch != "" {
		return core.Block, core.SafetyMessage(in, core.Block, forcePushWhat, "", forcePushHint), "force push of " + branch
	}
	if p := interpreterWrite(plain, isLeaf(in)); p != "" {
		hint := "Approve only if you asked for it; otherwise edit it by hand or run it yourself in a terminal."
		what := "an interpreter script that may write a file Edit/Write may never touch"
		return core.Ask, core.SafetyMessage(in, core.Ask, what, p, hint), "interpreter protected write"
	}
	return core.Allow, "", ""
}

// askPatternVerdict blocks hard rule 4 git verbs and hard rule 3 footers, and asks on destructive SQL.
func askPatternVerdict(in *core.Input, cmd string) (core.Decision, string, string) {
	if verb := harnessGitVerb(cmd, in.CWD); verb != "" {
		what := "git stash/checkout/switch/reset/restore/clean inside the shared config tree (CLAUDE.md hard rule 4)"
		if verb == shell.Uninspectable {
			what = "command nests too deep to check, inside the shared config tree (CLAUDE.md hard rule 4)"
		}
		hint := "The live checkout is shared; make a new commit to undo, or work in a linked worktree " +
			"(`claude-code worktree create <name>`)." + ownerRoute
		return core.Block, core.SafetyMessage(in, core.Block, what, "", hint), "shared config git mutation"
	}
	// A footer lives in the message body, so commit text stays; other data does not.
	if shell.AttributionFooterCommit(intentView(withPyLaunches(dropPyScripts(dropPatchBodies(in.ToolCommand()))), true), in.CWD) ||
		shell.FooterFileWritten(in.ToolCommand(), in.CWD) { // a -F file written earlier in the command, read unblanked
		what := "AI-attribution footer in a commit message (CLAUDE.md hard rule 3)"
		hint := "Remove the Co-Authored-By / Generated-with lines and commit again; that needs no approval." + ownerRoute
		return core.Block, core.SafetyMessage(in, core.Block, what, "", hint), "attribution footer"
	}
	if d, msg, detail := mainOnlyVerdict(in); d != core.Allow {
		return d, msg, detail
	}
	if sqlDestructive().MatchString(strings.ToLower(cmd)) {
		hint := "Approve only if you asked for it; back up first or run it against a scratch database."
		return core.Ask, core.SafetyMessage(in, core.Ask, "destructive SQL (DROP/TRUNCATE)", "", hint), "destructive SQL"
	}
	return core.Allow, "", ""
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
var pushValueFlags = sync.OnceValue(func() []string { return shell.GuardWords("git_push_value_flags") })

// impliedRefspec reports a push whose words are at most a remote and HEAD, so it updates the checked-out branch.
func impliedRefspec(args []string) bool {
	var words []string
	for i := 0; i < len(args); i++ {
		switch {
		case slices.Contains(pushValueFlags(), args[i]):
			i++
		case !strings.HasPrefix(args[i], "-"):
			words = append(words, args[i])
		}
	}
	return len(words) <= 1 || (len(words) == 2 && words[1] == "HEAD")
}

// protectedHead is dir's checked-out main/master, else ""; a failing git reports an unknown branch so the push asks.
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

// dropRemoteHeredocs removes quoted-delimiter heredoc bodies fed to ssh: they run remotely, unexpanded locally.
func dropRemoteHeredocs(cmd string) string {
	return core.DropHeredocBodies(cmd, false, func(lines []string, i int) int {
		m := core.HeredocOp().FindStringSubmatch(lines[i])
		if m == nil || m[4] != "" || strings.Contains(lines[i], "<<<") || !sshHoldsHeredoc(lines[i]) { // m[4]: bare delimiter
			return -1
		}
		return core.HeredocEnd(lines, i+1, m[2]+m[3]+m[4], m[1] == "-")
	})
}

// sshHoldsHeredoc reports an ssh segment carrying the heredoc operator whose output is not piped on.
func sshHoldsHeredoc(line string) bool {
	segs, seps := shell.SegmentsSep(line)
	for i, seg := range segs {
		if core.HeredocOp().MatchString(seg) {
			toks := shell.Tokens(seg)
			return len(toks) > 0 && toks[0] == "ssh" && seps[i] != "|" && seps[i] != "|&"
		}
	}
	return false
}

// fdToSubst matches a redirect to fd 3+, which `exec 3> >(sh)` can point at an interpreter.
var fdToSubst = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[<>]&[3-9]`) })

// inertStripped blanks data-only text but keeps command substitutions; pipeBlocks, exfil and SQL patterns match it.
func inertStripped(cmd string) string { return blankInert(dropInertHeredocs(cmd)) }

// blankInert is the shape rules' view: a segment is blanked only if it is inert and the intent view blanks it too.
func blankInert(cmd string) string {
	return blankSegments(cmd, false, func(segs, seps []string, i int, _ bool) bool {
		// A piped segment feeds the next command, so its text stays (`echo "curl x" | sh`) unless only filters read it.
		return inertSegment(segs[i]) && (seps[i] == "" || seps[i][0] != '|' || seps[i] == "||" || filtersOnly(segs, seps, i+1)) &&
			dataSegment(segs[i], false, false) && dataPipeline(segs, seps, i, false, false)
	})
}

// pipeFilters only read stdin and print it: none runs, writes or forwards its input.
var pipeFilters = sync.OnceValue(func() map[string]bool { return shell.GuardSet("pipe_filters") })

// filtersOnly reports a pipeline from segs[from] on made only of pipeFilters with no redirect, ending at the terminal.
func filtersOnly(segs, seps []string, from int) bool {
	for k := from; k < len(segs); k++ {
		toks := shell.Tokens(segs[k])
		if len(toks) == 0 || !pipeFilters()[toks[0]] || !inertEcho(toks) || !inertGrep(toks) {
			return false
		}
		if seps[k] != "|" && seps[k] != "|&" {
			return true
		}
	}
	return false
}

// msgHeredocOpen matches a line ending in `$(cat <<'EOF'`: the body becomes the substitution's text.
var msgHeredocOpen = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\$\(\s*cat\s+<<-?\s*(?:'[^']*'|"[^"]*"|[A-Za-z_][A-Za-z0-9_]*)\s*$`)
})

// msgSlot stands in for a dropped message body when judging the command it is an argument of.
const msgSlot = "HEREDOCMSG"

// dropMessageHeredocs drops each non-expanding `"$(cat <<'EOF' … EOF\n)"` body that is an unpiped inert message.
func dropMessageHeredocs(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	return core.DropHeredocBodies(cmd, false, func(lines []string, i int) int {
		loc := msgHeredocOpen().FindStringIndex(lines[i])
		ms := core.HeredocOp().FindAllStringSubmatch(lines[i], -1)
		if loc == nil || len(ms) != 1 {
			return -1
		}
		m := ms[0]
		end := core.HeredocEnd(lines, i+1, m[2]+m[3]+m[4], m[1] == "-")
		if end < 0 || end+1 >= len(lines) || m[4] != "" && slices.ContainsFunc(lines[i+1:end], substitutes) {
			return -1
		}
		closer, ok := strings.CutPrefix(strings.TrimLeft(lines[end+1], " \t"), ")")
		if !ok || !messageArg(lines[i][:loc[0]]+msgSlot+closer) {
			return -1
		}
		return end
	})
}

// messageArg reports that msgSlot sits in an inert segment whose output is not piped on.
func messageArg(flat string) bool {
	segs, seps := shell.SegmentsSep(flat)
	for k, seg := range segs {
		if strings.Contains(seg, msgSlot) {
			return inertSegment(seg) && seps[k] != "|" && seps[k] != "|&"
		}
	}
	return false
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

// inertScm: the --title of `claude-code scm pr create` is data; a --body-file path or a redirect is not.
func inertScm(toks []string) bool {
	if len(toks) < 4 || toks[1] != "scm" || !inertEcho(toks) ||
		slices.ContainsFunc(toks, func(t string) bool { return strings.HasPrefix(t, "--body-file") }) {
		return false
	}
	return toks[2] == "pr" && toks[3] == "create"
}

// dropPatchBodies drops each apply_patch call's patch text unless cmd runs a file or names apply_patch elsewhere.
func dropPatchBodies(cmd string) string {
	if !strings.Contains(cmd, "apply_patch") {
		return cmd
	}
	n := 0
	out := core.DropHeredocBodies(cmd, false, func(lines []string, i int) int {
		end := patchHeredocEnd(lines, i)
		if end > i {
			n++
		}
		return end
	})
	out, args := dropPatchArgs(out)
	if n += args; n == 0 || strings.Count(out, "apply_patch") != n || core.RunsWrittenFile().MatchString(out) {
		return cmd
	}
	return out
}

// patchHeredocEnd is the closing line of a `*** Begin Patch` heredoc fed to an unpiped `apply_patch`, else -1.
func patchHeredocEnd(lines []string, i int) int {
	line := lines[i]
	ms := core.HeredocOp().FindAllStringSubmatch(line, -1)
	if len(ms) != 1 || i+1 >= len(lines) || strings.TrimSpace(lines[i+1]) != "*** Begin Patch" ||
		strings.Contains(line, "<<<") || strings.Contains(line, ">(") || strings.Contains(line, "<(") {
		return -1
	}
	segs, seps := shell.SegmentsSep(line)
	for k, seg := range segs {
		if !core.HeredocOp().MatchString(seg) {
			continue
		}
		if !patchCall(seg, core.HeredocOp().ReplaceAllString(seg, " "), seps[k]) {
			return -1
		}
		m := ms[0]
		end := core.HeredocEnd(lines, i+1, m[2]+m[3]+m[4], m[1] == "-")
		if end < 0 || m[4] != "" && slices.ContainsFunc(lines[i+1:end], substitutes) { // a bare delimiter expands
			return -1
		}
		return end
	}
	return -1
}

// dropPatchArgs replaces each `apply_patch '<patch>'` segment's literal patch argument with nothing.
func dropPatchArgs(cmd string) (string, int) {
	segs, seps := shell.SegmentsSep(cmd)
	var b strings.Builder
	n := 0
	for i, seg := range segs {
		if toks := shell.Tokens(seg); len(toks) == 2 && strings.HasPrefix(toks[1], "*** Begin Patch") &&
			literalWord(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(seg), "apply_patch"))) &&
			patchCall(seg, seg, seps[i]) {
			seg = seg[:len(seg)-len(strings.TrimLeft(seg, " \t"))] + "apply_patch"
			n++
		}
		b.WriteString(seg)
		b.WriteString(seps[i])
	}
	return b.String(), n
}

// patchCall reports a segment whose command word is a bare `apply_patch` and whose output is not piped on.
func patchCall(seg, words, sep string) bool {
	toks := shell.Tokens(words)
	return len(toks) > 0 && toks[0] == "apply_patch" && strings.HasPrefix(strings.TrimLeft(seg, " \t"), "apply_patch ") &&
		sep != "|" && sep != "|&"
}

// literalWord reports one quoted word the shell passes as written: '…' alone, or "…" with no substitution.
func literalWord(w string) bool {
	if len(w) < 2 || w[len(w)-1] != w[0] {
		return false
	}
	return w[0] == '\'' && strings.Count(w, "'") == 2 || w[0] == '"' && !substitutes(w)
}

// substitutes reports command substitution text, which an unquoted heredoc or a "…" word runs.
func substitutes(s string) bool { return strings.Contains(s, "$(") || strings.Contains(s, "`") }

// dropInertHeredocs removes file-only heredoc bodies unless the command also runs a file.
func dropInertHeredocs(cmd string) string {
	return core.DropHeredocBodies(cmd, true, func(lines []string, i int) int {
		delim, quoted, dash, ok := core.InertHeredocStart(lines[i])
		if !ok {
			return -1
		}
		end := core.HeredocEnd(lines, i+1, delim, dash)
		// An unquoted delimiter expands substitutions in the body, so a body holding one is not inert.
		if end < 0 || !quoted && slices.ContainsFunc(lines[i+1:end], substitutes) {
			return -1
		}
		return end
	})
}

// extraProtected are Edit-protected files the settings template does not list.
var extraProtected = []string{"~/.claude/settings.local.json"}

// isLeaf reports an Agent-tool subagent (agent_id) or a dispatched CLI leaf (dispatch id or depth in the env).
func isLeaf(in *core.Input) bool { return in.AgentID != "" || proc.InDispatch(context.Background()) }

// leafSettings spells settings.json from $HOME, ~, a HOME concatenation or CLAUDE_CONFIG_DIR; a leaf may not write it.
var leafSettings = regexp.MustCompile(`(?i)(?:\$\{?home\}?|~|\bhome\b[^/\n]{0,12}|homedir\(\)[^/\n]{0,12}|expanduser\([^/\n]{0,4}~)` +
	`/\.claude/settings(?:\.local)?\.json|claude_config_dir[^\n]{0,40}/settings(?:\.local)?\.json`)

// protectedGlobs are the live settings.json `Edit(...)` deny globs (template's if unreadable) plus extraProtected.
func protectedGlobs(leaf bool) []string {
	globs := append([]string(nil), extraProtected...)
	if leaf { // a leaf never writes the harness settings, whatever the live deny list names
		globs = append(globs, filepath.Join(paths.ConfigDir(), "settings.json"), filepath.Join(paths.ConfigDir(), "settings.local.json"))
	}
	for _, rule := range liveDeny() {
		if g, ok := strings.CutPrefix(rule, "Edit("); ok && strings.HasSuffix(g, ")") {
			globs = append(globs, strings.TrimSuffix(g, ")"))
		}
	}
	return globs
}

// liveDeny is the live deny list; a missing or broken settings.json falls back to the template's so the guard stays on.
func liveDeny() []string {
	if live, err := settings.LivePermissions(); err == nil {
		return live.Deny
	}
	var doc struct {
		Permissions settings.PermissionLists `json:"permissions"`
	}
	if json.Unmarshal(settings.Template(), &doc) != nil {
		return nil // the embedded template is tested; the extras still guard
	}
	return doc.Permissions.Deny
}

// protectedWrite is the first Edit-denied path a command writes or removes ("" if none); temp folders are allowed.
func protectedWrite(cmd, cwd string, leaf bool) string {
	for _, p := range core.WrittenPaths(cmd, cwd) {
		if (protectedPath(p, leaf) || leaf && leafSettings.MatchString(p)) && !tempWriteAllowed(cmd, p) {
			return p
		}
	}
	for _, p := range core.RemovedPaths(cmd, cwd) {
		if protectedPath(p, leaf) && !removalAllowed(cmd, cwd) {
			return p
		}
	}
	return ""
}

// protectedPath reports whether p, or its symlink-resolved form, falls under an Edit deny glob.
func protectedPath(p string, leaf bool) bool {
	return slices.ContainsFunc([]string{p, paths.Real(p)}, func(q string) bool { return matchesProtected(q, leaf) })
}

// interpreterWrites are the per-language write indicators; reads (open(p), readFile) match none.
var interpreterWrites = sync.OnceValue(func() map[string]*regexp.Regexp {
	return map[string]*regexp.Regexp{
		"python": regexp.MustCompile(`open\s*\([^\n]*?(,\s*|mode\s*=\s*)['"][rbt]*[wax+][rwxabt+]*['"]|` +
			`\b(write_text|write_bytes|unlink|copy\w*|move|replace|rename|remove|rmtree)\s*\(`),
		"js": regexp.MustCompile(`\b(writeFile|appendFile|createWriteStream|rename|unlink|rm|copyFile|truncate|writeTextFile|remove)` +
			`(Sync)?\s*\(|Bun\.write\b`),
		"script": regexp.MustCompile(`open\b[^\n]*(['"]\+?>>?|['"]\+?[wa]\+?['"])|` +
			`\b(File|IO)\.(binwrite|write|delete|unlink|rename)\b|\b(unlink|rename)\s*[(\s]|FileUtils\.`),
	}
})

// interpreterLang maps an interpreter's basename to its interpreterWrites key ("" when not an interpreter).
func interpreterLang(base string) string {
	switch {
	case strings.HasPrefix(base, "python"):
		return "python"
	case base == "node" || base == "deno" || base == "bun":
		return "js"
	case base == "perl" || base == "ruby":
		return "script"
	}
	return ""
}

// interpreterWrite is the protected file an interpreter command's text mentions beside a write indicator, else "".
func interpreterWrite(cmd string, leaf bool) string {
	cmd = core.DropDataHeredocs(cmd)
	used := paths.FoldCase(pyUsedText(cmd)) // a Python comment or prose string only mentions a path
	for _, seg := range shell.Segments(cmd) {
		rest := runnerUnwrap(shell.Tokens(seg))
		if len(rest) == 0 {
			continue
		}
		lang := interpreterLang(filepath.Base(rest[0]))
		if re := interpreterWrites()[lang]; re == nil || !scriptWrites(lang, re, cmd) {
			continue
		}
		for _, g := range protectedGlobs(leaf) {
			needle := strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(g, "**/"), "**"), "~/")
			if needle != "" && strings.Contains(used, paths.FoldCase(needle)) {
				return needle
			}
		}
		if m := leafSettings.FindString(used); leaf && m != "" {
			return m
		}
	}
	return ""
}

// pyIndirectModule spots os/shutil reached by another name (`as`, `fs = os`, `(os)`, `modules['os']`) or dynamically.
var pyIndirectModule = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`\bimport\s[^;\n]*\b(os|shutil|posix|nt)\s+as\b|` +
		`\bfrom\s+(os|shutil|posix|nt)\s+import\b|__import__|import_module|getattr|` +
		`[=(\[]\s*(os|shutil|posix|nt)\s*(?:[;\n,)\]]|$)|['"](os|shutil|posix|nt)['"]`)
})

// scriptWrites reports a write indicator for lang in cmd; a Python str/list .replace(a, b)/.remove(x) is none.
func scriptWrites(lang string, re *regexp.Regexp, cmd string) bool {
	if lang != "python" || pyIndirectModule().MatchString(cmd) {
		return re.MatchString(cmd)
	}
	for _, m := range re.FindAllStringSubmatchIndex(cmd, -1) {
		if m[4] < 0 || !pyDataCall(cmd, m[4], m[5], m[1]) { // m[4:6]: the write-call name group
			return true
		}
	}
	return false
}

// pyDataCall reports str.replace(a, b) or list.remove(x), not os.replace or one-argument Path.replace(target).
func pyDataCall(cmd string, start, end, args int) bool {
	name := cmd[start:end]
	if name != "replace" && name != "remove" || start == 0 || cmd[start-1] != '.' {
		return false
	}
	qual := cmd[:start-1]
	notIdent := func(r rune) bool { return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) }
	qual = qual[strings.LastIndexFunc(qual, notIdent)+1:]
	if slices.Contains([]string{"os", "shutil", "posix", "nt"}, qual) {
		return false
	}
	n, ok := pyArgCount(cmd, args)
	if name == "replace" {
		return ok && n >= 2
	}
	return ok && n >= 1
}

// pyArgCount counts the top-level arguments of the call whose `(` ends at cmd[from-1]; ok=false when unclosed.
func pyArgCount(cmd string, from int) (n int, ok bool) {
	depth, quote, empty := 0, byte(0), true
	for i := from; i < len(cmd); i++ {
		c := cmd[i]
		closer := strings.IndexByte(")]}", c) >= 0
		switch {
		case quote != 0 && c == '\\':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote, empty = c, false
		case strings.IndexByte("([{", c) >= 0:
			depth, empty = depth+1, false
		case closer && depth > 0:
			depth--
		case closer:
			if empty {
				return 0, true
			}
			return n + 1, true
		case c == ',' && depth == 0:
			n++
		case c != ' ' && c != '\t':
			empty = false
		}
	}
	return 0, false
}

// matchesProtected reports whether abs falls under a deny glob: `**/x` by suffix, `dir/**` by prefix, else exact.
func matchesProtected(abs string, leaf bool) bool {
	abs = paths.FoldCase(abs)
	for _, g := range protectedGlobs(leaf) {
		g = paths.FoldCase(paths.ExpandHome(g))
		switch {
		case strings.HasPrefix(g, "**/"):
			if strings.HasSuffix(abs, string(filepath.Separator)+g[3:]) {
				return true
			}
		case strings.HasSuffix(g, "/**"):
			if paths.Within(abs, strings.TrimSuffix(g, "/**")) {
				return true
			}
		case abs == g:
			return true
		}
	}
	return false
}
