package hooks

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

// GrepExit (PostToolUseFailure Bash) notes that exit 1 from a last-stage grep means no match; it never blocks.
func GrepExit(in *core.Input) core.Result {
	if in.ToolName != "Bash" || !exitedOne(in.Nested("error")) {
		return core.Result{}
	}
	tool := lastStageGrep(in.ToolCommand(), in.CWD)
	if tool == "" {
		return core.Result{}
	}
	return core.Result{Context: "exit 1 from " + tool + " means no matches, not a failure: do not re-run it; " +
		"add `|| true` to count/existence checks."}
}

// exitedOne reports a first line of exactly `Exit code 1` (optionally after `Error: `), not 10, 127 and the like.
func exitedOne(errText string) bool {
	first, _, _ := strings.Cut(strings.TrimSpace(errText), "\n")
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(first), "Error: ")) == "Exit code 1"
}

// lastStageGrep names the grep tool the command's final pipeline stage runs, else "".
func lastStageGrep(cmd, cwd string) string {
	segs := shell.Segments(dropHeredocBodies(cmd))
	for _, seg := range slices.Backward(segs) {
		toks := shell.DropRtk(shell.Tokens(seg))
		for len(toks) > 0 && strings.Contains(toks[0], "=") && !strings.HasPrefix(toks[0], "-") {
			toks = toks[1:] // leading VAR=value assignments
		}
		if len(toks) > 0 {
			return grepName(toks, cwd)
		}
		if strings.TrimSpace(seg) != "" {
			return "" // an assignment-only last segment decides: no note
		}
	}
	return ""
}

func grepName(toks []string, cwd string) string {
	switch name := filepath.Base(toks[0]); name {
	case "grep", "rg", "egrep", "fgrep":
		return name
	case "git":
		if _, verb, _ := shell.GitInvocation(toks[1:], cwd); verb == "grep" {
			return "git grep"
		}
	}
	return ""
}

var heredocOpen = regexp.MustCompile(`<<-?\s*['"]?(\w+)`)

// dropHeredocBodies removes each heredoc body and its terminator line so the body never reads as commands.
func dropHeredocBodies(cmd string) string {
	var out []string
	end := ""
	for line := range strings.SplitSeq(cmd, "\n") {
		if end != "" {
			if strings.TrimSpace(line) == end {
				end = ""
			}
			continue
		}
		if m := heredocOpen.FindStringSubmatch(line); m != nil && !strings.Contains(line, "<<<") {
			end = m[1]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
