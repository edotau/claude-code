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
		// A piped segment feeds the next command, so its text stays: `echo "curl x" | sh` runs curl.
		if inertSegment(seg) && (seps[i] == "" || seps[i][0] != '|' || seps[i] == "||") {
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
