package safety

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

// ownerRoute ends every intent-rule deny: the owner's deliberate path past the hook.
const ownerRoute = " The owner can run it with `!` in the prompt."

// intentView is cmd minus data heredoc bodies and data words; keepCommit keeps commit text for the footer rule.
func intentView(cmd string, keepCommit bool) string {
	return blankData(dropDataBodies(cmd, keepCommit), keepCommit)
}

// dropDataBodies drops heredoc bodies fed to a data command (cat > f, a brief to an agent) unless a file runs.
func dropDataBodies(cmd string, keepCommit bool) string {
	if _, ok := heredocBodies(cmd); !ok {
		return cmd
	}
	return core.DropHeredocBodies(cmd, true, func(lines []string, i int) int {
		line := lines[i]
		ms := core.HeredocOp().FindAllStringSubmatch(line, -1)
		if len(ms) != 1 || strings.Contains(line, "<<<") || strings.Contains(line, ">(") || strings.Contains(line, "<(") {
			return -1
		}
		m := ms[0]
		end := core.HeredocEnd(lines, i+1, m[2]+m[3]+m[4], m[1] == "-")
		if end < 0 || m[4] != "" && slices.ContainsFunc(lines[i+1:end], substitutes) || !heredocFeedsData(line, keepCommit) {
			return -1
		}
		return end
	})
}

// heredocFeedsData reports a heredoc whose command, and every command it pipes into, only stores or reads text.
func heredocFeedsData(line string, keepCommit bool) bool {
	segs, seps := shell.SegmentsSep(line)
	for k, seg := range segs {
		if core.HeredocOp().MatchString(seg) {
			return dataSegment(core.HeredocOp().ReplaceAllString(seg, " "), false, keepCommit) &&
				dataPipeline(segs, seps, k, false, keepCommit)
		}
	}
	return false
}

// bodySlot stands in for a kept heredoc body while blankData judges the lines around it.
const bodySlot = "\x00HEREDOCBODY"

// blankData blanks each data segment's words (substitutions kept); kept heredoc bodies are never judged as shell.
func blankData(cmd string, keepCommit bool) string {
	return blankSegments(cmd, true, func(segs, seps []string, i int, runs bool) bool {
		return dataSegment(segs[i], runs, keepCommit) && dataPipeline(segs, seps, i, runs, keepCommit)
	})
}

// blankSegments cuts each segment blank picks to its substitutions; lift keeps heredoc bodies out of the segments.
func blankSegments(cmd string, lift bool, blank func(segs, seps []string, i int, runs bool) bool) string {
	if strings.Contains(cmd, ">(") || strings.Contains(cmd, "<(") || fdToSubst().MatchString(cmd) {
		return cmd
	}
	outer, bodies, runs := cmd, []string(nil), false
	if lift {
		var ok bool
		if outer, bodies, ok = liftBodies(cmd); !ok {
			return cmd
		}
		runs = core.RunsWrittenFile().MatchString(outer)
	}
	segs, seps := shell.SegmentsSep(outer)
	var b strings.Builder
	for i, seg := range segs {
		if blank(segs, seps, i, runs) {
			seg = keptSubstitutions(seg)
		}
		b.WriteString(seg)
		b.WriteString(seps[i])
	}
	return spliceBodies(b.String(), bodies)
}

// liftBodies replaces each heredoc body with a numbered bodySlot line and returns the bodies.
func liftBodies(cmd string) (string, []string, bool) {
	spans, ok := heredocBodies(cmd)
	if len(spans) == 0 {
		return cmd, nil, ok
	}
	lines := strings.Split(cmd, "\n")
	var out, bodies []string
	last := 0
	for _, sp := range spans {
		out = append(out, lines[last:sp[0]]...)
		out = append(out, fmt.Sprintf("%s%d", bodySlot, len(bodies)))
		bodies = append(bodies, strings.Join(lines[sp[0]:sp[1]], "\n")+"\n")
		last = sp[1]
	}
	return strings.Join(append(out, lines[last:]...), "\n"), bodies, true
}

// heredocBodies are the [from, to) line spans of cmd's heredoc bodies; ok=false when an operator sits in a quote.
func heredocBodies(cmd string) (spans [][2]int, ok bool) {
	if !strings.Contains(cmd, "<<") {
		return nil, true
	}
	lines := strings.Split(cmd, "\n")
	var outer strings.Builder
	for i := 0; i < len(lines); i++ {
		m := core.HeredocOp().FindStringSubmatchIndex(lines[i])
		if m != nil && !strings.Contains(lines[i], "<<<") && quotedOp(outer.String(), lines[i], m[0]) {
			return nil, false // the operator is quoted text: the lines are not what they seem
		}
		outer.WriteString(lines[i])
		outer.WriteString("\n")
		if m == nil || strings.Contains(lines[i], "<<<") {
			continue
		}
		g := core.HeredocOp().FindStringSubmatch(lines[i])
		if end := core.HeredocEnd(lines, i+1, g[2]+g[3]+g[4], g[1] == "-"); end > i+1 {
			spans = append(spans, [2]int{i + 1, end})
			i = end - 1
		}
	}
	return spans, true
}

// quotedOp reports line[at] inside a quote, unless it opens a `"$(cat <<'EOF'` substitution, which is code.
func quotedOp(before, line string, at int) bool {
	q := shell.QuoteAt(before + line)[len(before)+at]
	k := strings.LastIndex(line[:at], "$(")
	return q == '\'' || q == '"' && (k < 0 || strings.Contains(line[k:at], ")"))
}

// dataPipeline reports that segs[i]'s output, if piped, reaches only data commands.
func dataPipeline(segs, seps []string, i int, runs, keepCommit bool) bool {
	for k := i; seps[k] == "|" || seps[k] == "|&"; k++ {
		if k+1 >= len(segs) || !dataSegment(segs[k+1], runs, keepCommit) {
			return false
		}
	}
	return true
}

// dataCommands only print, search, store or hand on their words as text; none runs them as a command.
var dataCommands = sync.OnceValue(func() map[string]bool { return shell.GuardSet("data_commands") })

// dataHarnessVerbs are the claude-code verbs whose words are briefs, search terms, facts or PR text.
var dataHarnessVerbs = sync.OnceValue(func() map[string]bool { return shell.GuardSet("data_harness_verbs") })

// dataSegment reports a segment whose words are text; a redirect counts only when no file runs in the same line.
func dataSegment(seg string, runs, keepCommit bool) bool {
	toks := dropEnvAssigns(shell.Tokens(seg))
	if len(toks) == 0 || runs && slices.ContainsFunc(toks, func(t string) bool { return strings.Contains(t, ">") }) {
		return false
	}
	name := filepath.Base(toks[0])
	if name == "claude-code" {
		return claudeCodeData(toks, keepCommit)
	}
	if judge, ok := dataJudges[name]; ok {
		return judge(toks)
	}
	return dataCommands()[name] || name == "git" && !keepCommit && inertGit(toks)
}

// dropEnvAssigns removes the leading NAME=value words.
func dropEnvAssigns(toks []string) []string {
	for len(toks) > 0 && envAssign().MatchString(toks[0]) {
		toks = toks[1:]
	}
	return toks
}

// claudeCodeData reports a claude-code segment of text: a data verb, or a worktree commit message unless keepCommit.
func claudeCodeData(toks []string, keepCommit bool) bool {
	return len(toks) > 1 && dataHarnessVerbs()[toks[1]] || worktreeText(toks) || !keepCommit && inertWorktreeCommit(toks)
}

// worktreeText: the words of `claude-code worktree pr|checks|comment` are PR text, as `scm` was.
func worktreeText(toks []string) bool {
	return len(toks) > 2 && toks[1] == "worktree" && (toks[2] == "pr" || toks[2] == "checks" || toks[2] == "comment")
}

// dataJudges decide per command whether its words are text: grep without a --pre, sed without `e`, PR text, briefs.
var dataJudges = map[string]func(toks []string) bool{"grep": inertGrep, "egrep": inertGrep, "fgrep": inertGrep,
	"rg": inertGrep, "ag": inertGrep, "gh": inertGh,
	"sed": func(toks []string) bool { return !slices.ContainsFunc(toks[1:], sedExec().MatchString) },
}

// envAssign matches a leading NAME=value word.
var envAssign = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`) })

// sedExec spots GNU sed's `e` command or an `s///e` flag, which run text as a shell command.
var sedExec = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|[;{}\n])\s*e(\s|;|$)|/[gpIiMm0-9w]*e[gpIiMm0-9w]*\s*($|[;}])`)
})

// harmlessGit is a rule-2 verb form that discards and hides no work: a new branch, or an unstage.
func harmlessGit(verb string, args []string) bool {
	switch verb {
	case "checkout":
		return newBranchOnly(args, "-b")
	case "switch":
		return newBranchOnly(args, "-c", "--create")
	case "restore":
		return unstageOnly(args)
	case "reset":
		return resetUnstage(args)
	}
	return false
}

// branchQuietFlags are the checkout/switch flags that neither discard nor overwrite.
var branchQuietFlags = sync.OnceValue(func() map[string]bool { return shell.GuardSet("branch_quiet_flags") })

// newBranchOnly reports a create flag with its name, at most one start point and only quiet/track flags.
func newBranchOnly(args []string, create ...string) bool {
	named, points := false, 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case slices.Contains(create, a) && i+1 < len(args):
			named = true
			i++
		case !strings.HasPrefix(a, "-"):
			points++
		case !branchQuietFlags()[a] && !strings.HasPrefix(a, "--track="):
			return false // -f, -B, --discard-changes, --orphan, -- paths …
		}
	}
	return named && points <= 1
}

// unstageOnly reports `git restore --staged` with no --worktree: it resets the index and leaves files alone.
func unstageOnly(args []string) bool {
	staged := false
	for i := 0; i < len(args) && args[i] != "--"; i++ {
		a := args[i]
		if a == "-s" || a == "--source" {
			i++
			continue
		}
		if !restoreIndexFlag(a) {
			return false
		}
		staged = staged || a == "--staged" || !strings.HasPrefix(a, "--") && strings.Contains(a, "S")
	}
	return staged
}

// restoreIndexFlag is a pathspec or a restore flag that touches only the index: -S, --staged, -q, --source=.
func restoreIndexFlag(a string) bool {
	switch {
	case !strings.HasPrefix(a, "-"):
		return true
	case strings.HasPrefix(a, "--"):
		return a == "--staged" || a == "--quiet" || strings.HasPrefix(a, "--source=")
	}
	return strings.Trim(a[1:], "Sq") == ""
}

// resetUnstage reports a mixed `git reset` of HEAD itself, optionally limited to paths: it only unstages.
func resetUnstage(args []string) bool {
	head := false
	for i, a := range args {
		switch {
		case a == "--":
			return !slices.ContainsFunc(args[i+1:], func(p string) bool { return strings.HasPrefix(p, "-") })
		case a == "-q" || a == "--quiet" || a == "--mixed":
		case a == "HEAD" && !head:
			head = true
		case !head || strings.HasPrefix(a, "-"): // a commit other than HEAD, or a mode such as --hard
			return false
		}
	}
	return true
}

const sudoHint = "sudo is not run from an agent session; install to a user prefix (~/.local, pipx, a venv) instead." +
	ownerRoute

// sudoInvoked reports sudo/doas in command position in view, in text it feeds a shell, or in an interpreter's code.
func sudoInvoked(view, cwd string) bool {
	if !strings.Contains(view, "sudo") && !strings.Contains(view, "doas") {
		return false
	}
	_, texts := shellFeeds(view)
	for _, s := range shell.RunScripts(view, cwd) {
		texts = append(texts, s.Text)
	}
	return sudoRuns(view, 0) || slices.ContainsFunc(texts, func(t string) bool { return sudoRuns(t, 1) }) ||
		interpreterBodySudo(view)
}

// sudoWord matches sudo or doas as a word, the test for code an interpreter may hand to a shell.
var sudoWord = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(^|[^[:alnum:]_-])(sudo|doas)(\s|$)`) })

// sudoRuns reports sudo/doas leading a command anywhere text runs one, substitutions and nested strings included.
func sudoRuns(text string, depth int) bool {
	if depth > 4 {
		return sudoWord().MatchString(text) // too deep to model: fail closed on the word
	}
	masked, subs := shell.ExtractSubstitutions(text)
	if slices.ContainsFunc(subs, func(s string) bool { return sudoRuns(s, depth+1) }) {
		return true
	}
	for _, seg := range shell.Segments(masked) {
		if sudoLeads(shell.Tokens(seg), depth) {
			return true
		}
	}
	return false
}

// sudoPrefixes run the command after their options; a sudo word among those options' values also counts.
var sudoPrefixes = sync.OnceValue(func() map[string]bool {
	out := shell.GuardSet("sudo_prefix_extras")
	maps.Copy(out, shell.GuardSet("wrapper_names"))
	maps.Copy(out, shell.GuardSet("shell_keywords"))
	delete(out, "sudo") // sudoLeads tests sudo before this set
	return out
})

// sudoLeads reports a token sequence whose command, past prefixes and wrappers, is sudo or runs a string holding it.
func sudoLeads(toks []string, depth int) bool {
	for len(toks) > 0 {
		toks = shell.DropRtk(shellSkipAssign(toks))
		if len(toks) == 0 {
			return false
		}
		switch name := filepath.Base(toks[0]); {
		case name == "sudo" || name == "doas":
			return true
		case sudoPrefixes()[name]:
			if slices.ContainsFunc(toks[1:], func(t string) bool {
				return t == "sudo" || t == "doas" || strings.Contains(t, " ") && sudoRuns(t, depth+1)
			}) {
				return true // `nice -n 5 sudo x`, env -S 'sudo x', flock f -c 'sudo x', watch 'sudo x'
			}
			toks = skipPrefixOpts(name, toks)
		case name == "ssh":
			return slices.ContainsFunc(toks[1:], func(t string) bool { return sudoRuns(t, depth+1) })
		case name == "find":
			i := slices.IndexFunc(toks, func(t string) bool { return t == "-exec" || t == "-execdir" || t == "-ok" })
			if i < 0 {
				return false
			}
			toks = toks[i+1:]
		default:
			if nested, ok := shell.NestedCommand(toks); ok {
				return sudoRuns(nested, depth+1)
			}
			return interpreterLang(name) != "" && slices.ContainsFunc(toks[1:], sudoWord().MatchString)
		}
	}
	return false
}

// skipPrefixOpts drops a prefix command's options, and timeout's duration; a value option's value is checked next round.
func skipPrefixOpts(name string, toks []string) []string {
	if name == "xargs" {
		return agent.SkipXargs(toks)
	}
	toks = toks[1:]
	for len(toks) > 0 && (strings.HasPrefix(toks[0], "-") || len(toks) > 1 && name == "timeout") {
		toks = toks[1:]
	}
	return toks
}

// shellSkipAssign drops leading NAME=value words.
func shellSkipAssign(toks []string) []string {
	for len(toks) > 0 && envAssign().MatchString(toks[0]) {
		toks = toks[1:]
	}
	return toks
}

// interpreterBodySudo reports sudo/doas as a word in a heredoc body fed to python, node, perl or ruby.
func interpreterBodySudo(view string) bool {
	spans, _ := heredocBodies(view)
	lines := strings.Split(view, "\n")
	for _, sp := range spans {
		for _, seg := range shell.Segments(lines[sp[0]-1]) {
			rest, _ := shell.Unwrap(shell.Tokens(seg), "")
			if core.HeredocOp().MatchString(seg) && len(rest) > 0 && interpreterLang(filepath.Base(rest[0])) != "" &&
				sudoWord().MatchString(strings.Join(lines[sp[0]:sp[1]], "\n")) {
				return true
			}
		}
	}
	return false
}

// hardDataCommands never run their words: a hard-block shape only in their arguments is text, not a command.
var hardDataCommands = map[string]func(toks []string) bool{
	"grep": inertGrep, "rg": inertGrep, "ag": inertGrep, "echo": inertWords, "printf": inertWords,
	"jq": inertWords, "test": inertWords, "[": inertWords,
	"git": func(toks []string) bool { return inertGit(toks) || len(toks) > 1 && toks[1] == "log" },
	"claude-code": func(toks []string) bool {
		return len(toks) > 2 && toks[1] == "agents" && slices.Contains(briefVerbs(), toks[2])
	},
}

func inertWords([]string) bool { return true }

var briefVerbs = sync.OnceValue(func() []string { return shell.GuardWords("agents_brief_verbs") })

// runnerNames take their input or arguments as code; text piped into one is not data.
var runnerNames = sync.OnceValue(func() map[string]bool { return shell.GuardSet("runner_names") })

// blankHardData blanks the words of hardDataCommands segments outside heredoc bodies, keeping substitutions.
func blankHardData(cmd string) string {
	return blankSegments(cmd, true, func(segs, seps []string, i int, runs bool) bool {
		return hardDataSegment(segs[i], runs) && !pipesIntoRunner(segs, seps, i)
	})
}

// hardDataSegment reports a hardDataCommands segment; a redirect counts only when no file runs in the same line.
func hardDataSegment(seg string, runs bool) bool {
	toks := shell.Tokens(seg)
	if len(toks) == 0 || runs && slices.ContainsFunc(toks, func(t string) bool { return strings.Contains(t, ">") }) {
		return false
	}
	judge, ok := hardDataCommands[filepath.Base(toks[0])]
	return ok && judge(toks)
}

// pipesIntoRunner reports segs[i]'s output reaching a runner or interpreter through a pipe.
func pipesIntoRunner(segs, seps []string, i int) bool {
	for k := i; k+1 < len(segs) && (seps[k] == "|" || seps[k] == "|&"); k++ {
		rest, _ := shell.Unwrap(shell.Tokens(segs[k+1]), "")
		if len(rest) == 0 || runnerNames()[filepath.Base(rest[0])] || interpreterLang(filepath.Base(rest[0])) != "" {
			return true
		}
	}
	return false
}

// keptSubstitutions is seg reduced to its command substitutions, which still run.
func keptSubstitutions(seg string) string {
	_, subs := shell.ExtractSubstitutions(seg)
	var kept strings.Builder
	for _, sub := range subs {
		kept.WriteString(" $(")
		kept.WriteString(sub)
		kept.WriteString(")")
	}
	return kept.String()
}

// spliceBodies puts liftBodies' bodies back in place of their bodySlot lines.
func spliceBodies(out string, bodies []string) string {
	for k, body := range bodies {
		out = strings.Replace(out, fmt.Sprintf("%s%d\n", bodySlot, k), body, 1)
	}
	return out
}

// freshMktemp reports a `$(mktemp …)` at the start of s that makes its file under a system temp root, not the cwd.
func freshMktemp(s string) bool {
	inner, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimLeft(s, `"`), "$("), ")")
	toks := shell.Tokens(inner)
	if len(toks) == 0 || toks[0] != "mktemp" {
		return false
	}
	underTempDir := func(p string) bool { return filepath.IsAbs(p) && !strings.Contains(p, "..") && underTemp(p) }
	prefixed := false // -t: the template is a prefix inside the temp dir
	for i := 1; i < len(toks); i++ {
		switch a := toks[i]; {
		case a == "-p" || a == "--tmpdir":
			if i++; i >= len(toks) || !underTempDir(toks[i]) {
				return false
			}
		case strings.HasPrefix(a, "--tmpdir=") || strings.HasPrefix(a, "-p"):
			if !underTempDir(strings.TrimPrefix(strings.TrimPrefix(a, "--tmpdir="), "-p")) {
				return false
			}
		case a == "-t":
			prefixed = true
		case strings.HasPrefix(a, "-"):
			if strings.Trim(a[1:], "dqu") != "" && a != "--directory" && a != "--quiet" && a != "--dry-run" {
				return false
			}
		case !prefixed && !underTempDir(a) || strings.Contains(a, "/") && !underTempDir(a):
			return false // a bare or relative template lands in the cwd
		}
	}
	return true
}

// mktempAssign matches a statement assigning one `$(mktemp …)` substitution.
var mktempAssign = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(?:export\s+)?([A-Za-z_]\w*="?\$\(\s*mktemp[^)]*\)"?)$`)
})

// varPaths maps each variable a statement assigns a literal absolute path, or a fresh `$(mktemp)` dir, to that path.
func varPaths(cmd string) map[string]string {
	vals, bad := map[string]string{}, map[string]bool{}
	for _, seg := range shell.Segments(cmd) {
		toks := shell.Tokens(seg)
		if m := mktempAssign().FindStringSubmatch(strings.TrimSpace(seg)); m != nil { // Tokens splits the substitution
			toks = []string{m[1]}
		}
		if len(toks) > 0 && (toks[0] == "export" || toks[0] == "local" || toks[0] == "declare") {
			toks = toks[1:]
		}
		if len(toks) == 0 || len(shellSkipAssign(toks)) > 0 { // `T=x cmd` sets T for cmd only
			continue
		}
		for _, t := range toks {
			name, word, _ := strings.Cut(t, "=")
			word = expandWith(word, vals)
			switch {
			case mktempCd[2].MatchString(word) && freshMktemp(word):
				word = filepath.Join(os.TempDir(), "mktemp-"+name) // a fresh dir: never the shared tree
			case !filepath.IsAbs(word) || strings.ContainsAny(word, "$`*?") || strings.Contains(word, ".."):
				bad[name] = true
			}
			if prev, seen := vals[name]; bad[name] || seen && prev != word {
				bad[name] = true
				delete(vals, name)
				continue
			}
			vals[name] = word
		}
	}
	return vals
}

// expandVars writes each varPaths variable's path in place of $NAME and ${NAME}.
func expandVars(text string) string {
	if !strings.Contains(text, "=") || !strings.Contains(text, "$") {
		return text
	}
	return expandWith(text, varPaths(text))
}

func expandWith(text string, vals map[string]string) string {
	for name, val := range vals {
		re := regexp.MustCompile(`\$(\{` + name + `\}|` + name + `\b)`)
		text = re.ReplaceAllLiteralString(text, val)
	}
	return text
}

// guardedWord matches a hard rule 4 git verb as a whole word anywhere in text.
var guardedWord = sync.OnceValue(func() *core.Gated {
	verbs := shell.GuardWords("git_shared_tree_banned_verbs")
	return core.Gate(`\b(?:`+strings.Join(verbs, "|")+`)\b`, verbs...)
})

// tooDeepNoVerb reports a too-deep call whose command text holds no guarded verb word, so nothing there can mutate.
func tooDeepNoVerb(verb string, args []string, cmd string) bool {
	return verb == shell.Uninspectable && shell.TooDeep(args) && !guardedWord().MatchString(cmd)
}
