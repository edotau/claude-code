package quality

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/shell"
)

// GrepExit (PostToolUseFailure Bash) notes that exit 1 from a last-stage grep means no match; it never blocks.
func GrepExit(in *core.Input) (res core.Result) {
	defer func() {
		if recover() != nil {
			res = core.Result{} // fail open: a parser panic gives no note
		}
	}()
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
		toks := dropAssigns(shell.DropRtk(dropAssigns(shell.Tokens(seg))))
		if len(toks) > 0 {
			return grepName(toks, cwd)
		}
		if strings.TrimSpace(seg) != "" {
			return "" // an assignment-only last segment decides: no note
		}
	}
	return ""
}

// dropAssigns removes leading VAR=value assignments.
func dropAssigns(toks []string) []string {
	for len(toks) > 0 && strings.Contains(toks[0], "=") && !strings.HasPrefix(toks[0], "-") {
		toks = toks[1:]
	}
	return toks
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

// dropHeredocBodies removes each heredoc body and its terminator line, and each comment, so neither reads as commands.
func dropHeredocBodies(cmd string) string {
	var out []string
	var pending []heredocWant
	for line := range strings.SplitSeq(cmd, "\n") {
		if len(pending) > 0 {
			if pending[0].closes(line) {
				pending = pending[1:]
			}
			continue
		}
		line = shell.DropComments(line)
		pending = heredocOpeners(line)
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

type heredocWant struct {
	delim string
	dash  bool
}

// closes: bash strips leading tabs only for `<<-`.
func (h heredocWant) closes(line string) bool {
	if h.dash {
		line = strings.TrimLeft(line, "\t")
	}
	return line == h.delim
}

// heredocOpeners lists the `<<[-]WORD` operators on one line, skipping quoted text, `<<<` and `((…))` shifts.
func heredocOpeners(line string) []heredocWant {
	var out []heredocWant
	at := shell.QuoteAt(line)
	arith := 0
	for i := 0; i < len(line); i++ {
		rest := line[i:]
		switch {
		case at[i] != 0:
		case strings.HasPrefix(rest, "(("), strings.HasPrefix(rest, "))") && arith > 0:
			arith, i = arith+arithStep(rest[0]), i+1
		case strings.HasPrefix(rest, "<<<"):
			i += 2
		case strings.HasPrefix(rest, "<<") && arith == 0:
			w, n := heredocOperand(rest[2:])
			if w.delim != "" {
				out = append(out, w)
			}
			i += 1 + n
		}
	}
	return out
}

// arithStep is +1 for an opening paren and -1 for a closing one.
func arithStep(c byte) int { return 1 - 2*int(c-'(') }

// heredocOperand parses what follows `<<` (optional `-`, blanks, a word) and the bytes it spans.
func heredocOperand(s string) (heredocWant, int) {
	w := heredocWant{dash: strings.HasPrefix(s, "-")}
	body := strings.TrimPrefix(s, "-")
	word := strings.TrimLeft(body, " \t")
	delim, n := heredocWord(word)
	w.delim = delim
	return w, len(s) - len(word) + n
}

// heredocWord reads one shell word with quotes and backslashes removed, and the bytes it spans.
func heredocWord(s string) (string, int) {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			b.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case strings.IndexByte(" \t;&|()<>", c) >= 0:
			return b.String(), i
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), len(s)
}
