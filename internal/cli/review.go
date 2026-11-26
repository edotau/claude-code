package cli

import (
	"io"
	"os"
	"slices"

	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/dev/review/bench"
	"github.com/edotau/claude-code/internal/dev/review/diff"
	"github.com/edotau/claude-code/internal/dev/review/source"
)

// reviewTools are the evaluate-code/simplify detectors, each keeping its former Python CLI.
var reviewTools = map[string]func(args []string, stdin io.Reader, stdout, stderr io.Writer) int{
	"quality":    func(a []string, _ io.Reader, o, e io.Writer) int { return source.RunQuality(a, o, e) },
	"complexity": func(a []string, _ io.Reader, o, e io.Writer) int { return source.RunComplexity(a, o, e) },
	"simplify":   func(a []string, _ io.Reader, o, e io.Writer) int { return source.RunSimplify(a, o, e) },
	"diff":       func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunDiff(a, o, e) },
	"pr":         func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunPR(a, o, e) },
	"report":     func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunReport(a, o, e) },
	"gate":       func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunGate(a, o, e) },
	"scope":      func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunScope(a, o, e) },
	"bench":      func(a []string, _ io.Reader, o, e io.Writer) int { return bench.RunBench(a, o, e) },
}

var reviewPage = helptext.Page{
	Usage: "claude-code review <tool> [args...]",
	About: "The evaluate-code and simplify detectors as one verb; each tool has its own flags.",
	Commands: [][2]string{
		{"quality", "code smells and SOLID violations in a file or tree"},
		{"complexity", "function length, nesting and branch counts"},
		{"simplify", "complexity + quality pass over the changed files"},
		{"diff", "diff lines that don't trace to the task"},
		{"pr", "a branch's changes by category, risk and review order"},
		{"report", "pr + quality findings as one review report"},
		{"gate", "advisory pre-commit summary of the staged change"},
		{"scope", "files a change or path set touches, sized in bytes"},
		{"bench", "run changed packages' benchmarks, compare with the baseline"},
	},
	Hint: "claude-code review <tool> -h lists that tool's flags.",
}

// cmdReview dispatches `review <tool>`.
func cmdReview(args []string) int {
	if len(args) == 0 || isHelpVerb(args[0]) {
		reviewPage.Fprint(os.Stdout)
		return 0
	}
	run := reviewTools[args[0]]
	if run == nil {
		return unknownSub("review", args[0], reviewPage)
	}
	return run(args[1:], os.Stdin, os.Stdout, os.Stderr)
}

// reviewToolNames lists the review detectors, derived from reviewTools so a new one is completable for free.
func reviewToolNames() []string {
	out := make([]string, 0, len(reviewTools))
	for n := range reviewTools {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// reviewSpec is the completion node for `review`: every detector, none with completable flags.
func reviewSpec() *helptext.Node {
	sub := map[string]*helptext.Node{}
	for _, n := range reviewToolNames() {
		sub[n] = &helptext.Node{}
	}
	return &helptext.Node{Sub: sub}
}
