package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/review/diff"
	"github.com/edotau/claude-code/internal/review/plan"
	"github.com/edotau/claude-code/internal/review/source"
)

// reviewTools are the evaluate-code/simplicity detectors, each keeping its former Python CLI.
var reviewTools = map[string]func(args []string, stdin io.Reader, stdout, stderr io.Writer) int{
	"quality":     func(a []string, _ io.Reader, o, e io.Writer) int { return source.RunQuality(a, o, e) },
	"complexity":  func(a []string, _ io.Reader, o, e io.Writer) int { return source.RunComplexity(a, o, e) },
	"assumptions": plan.RunAssumptions,
	"goals":       plan.RunGoals,
	"diff":        func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunDiff(a, o, e) },
	"pr":          func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunPR(a, o, e) },
	"report":      func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunReport(a, o, e) },
	"gate":        func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunGate(a, o, e) },
}

func cmdReview(args []string) int {
	names := make([]string, 0, len(reviewTools))
	for n := range reviewTools {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(args) == 0 || reviewTools[args[0]] == nil {
		fmt.Fprintf(os.Stderr, "usage: claude-code review <%s> [args...]  (each takes -h)\n", strings.Join(names, "|"))
		return 2
	}
	return reviewTools[args[0]](args[1:], os.Stdin, os.Stdout, os.Stderr)
}

func cmdWorkflow(args []string) int {
	if len(args) == 0 || args[0] != "skeleton" {
		fmt.Fprintln(os.Stderr, "usage: claude-code workflow skeleton <pipeline|parallel|evaluator|orchestrator> [--name <name>]")
		return 2
	}
	return plan.RunSkeleton(args[1:], os.Stdout, os.Stderr)
}
