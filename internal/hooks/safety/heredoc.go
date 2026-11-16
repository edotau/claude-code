package safety

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

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

// inertStripped blanks text that is only data (message values, echo/grep arguments, file-only heredoc bodies)
// but keeps every command substitution; pipeBlocks, exfilPatterns and sqlDestructive match this view.
func inertStripped(cmd string) string { return blankInert(dropInertHeredocs(cmd)) }

// blankInert is cmd with each inert segment's words blanked (substitutions kept), heredoc bodies untouched.
func blankInert(cmd string) string {
	if strings.Contains(cmd, ">(") || strings.Contains(cmd, "<(") || fdToSubst().MatchString(cmd) {
		return cmd
	}
	segs, seps := shell.SegmentsSep(cmd)
	var b strings.Builder
	for i, seg := range segs {
		// A piped segment feeds the next command, so its text stays (`echo "curl x" | sh`) unless only filters read it.
		if inertSegment(seg) && (seps[i] == "" || seps[i][0] != '|' || seps[i] == "||" || filtersOnly(segs, seps, i+1)) {
			_, subs := shell.ExtractSubstitutions(seg)
			var kept strings.Builder
			for _, sub := range subs {
				kept.WriteString(" $(")
				kept.WriteString(sub)
				kept.WriteString(")")
			}
			seg = kept.String()
		}
		b.WriteString(seg)
		b.WriteString(seps[i])
	}
	return b.String()
}

// pipeFilters only read stdin and print it: none runs, writes or forwards its input.
var pipeFilters = map[string]bool{"head": true, "tail": true, "wc": true, "cut": true, "tr": true, "grep": true,
	"rg": true, "jq": true}

// filtersOnly reports a pipeline from segs[from] on made only of pipeFilters with no redirect, ending at the terminal.
func filtersOnly(segs, seps []string, from int) bool {
	for k := from; k < len(segs); k++ {
		toks := shell.Tokens(segs[k])
		if len(toks) == 0 || !pipeFilters[toks[0]] || !inertEcho(toks) || !inertGrep(toks) {
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
		if end < 0 || !quoted && slices.ContainsFunc(lines[i+1:end], func(l string) bool {
			return strings.Contains(l, "$(") || strings.Contains(l, "`")
		}) {
			return -1
		}
		return end
	})
}
