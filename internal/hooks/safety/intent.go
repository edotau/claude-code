package safety

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/permission"
	"github.com/edotau/claude-code/internal/platform/shell"
)

// ownerRoute ends every deny: the owner's deliberate path past the hook.
var ownerRoute = assets.SafetyOwnerRoute()

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
	for k := i; pipedOn(seps[k]); k++ {
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
	if keepCommit && shell.PRMessage(toks) { // a PR's title and body are scanned for a footer, as a commit's are
		return false
	}
	if shell.IsHarnessName(name) {
		return claudeCodeData(toks, keepCommit)
	}
	return plainData(name, toks, keepCommit)
}

// plainData reports a non-harness command whose words are text.
func plainData(name string, toks []string, keepCommit bool) bool {
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
	if len(toks) > 2 && toks[1] == "agents" && toks[2] == "diff" {
		return false
	}
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

var sudoHint = assets.SafetySudoHint() + ownerRoute

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
		toks = shell.DropRtk(dropEnvAssigns(toks))
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
		return permission.SkipXargs(toks)
	}
	toks = toks[1:]
	for len(toks) > 0 && (strings.HasPrefix(toks[0], "-") || len(toks) > 1 && name == "timeout") {
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
