package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/diff"
	"github.com/edotau/claude-code/internal/review/plan"
	"github.com/edotau/claude-code/internal/review/source"
)

// reviewTools are the evaluate-code/simplify detectors, each keeping its former Python CLI.
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
	text := fmt.Sprintf("usage: claude-code review <tool> [args...]  (each takes -h)\n       tools: %s\n", strings.Join(names, "|"))
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		helptext.Fprint(os.Stdout, text)
		return 0
	}
	if len(args) == 0 || reviewTools[args[0]] == nil {
		helptext.Fprint(os.Stderr, text)
		return 2
	}
	return reviewTools[args[0]](args[1:], os.Stdin, os.Stdout, os.Stderr)
}

const workflowUsage = "usage: claude-code workflow skeleton <pipeline|parallel|evaluator|orchestrator>\n" +
	"                            [--name <name>]\n" +
	"       claude-code workflow lint [<root>]  check model/effort pins + meta/throw"

func cmdWorkflow(args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		helptext.Fprint(os.Stdout, workflowUsage+"\n")
		return 0
	}
	if len(args) == 0 {
		helptext.Fprint(os.Stderr, workflowUsage+"\n")
		return 2
	}
	switch args[0] {
	case "skeleton":
		return plan.RunSkeleton(args[1:], os.Stdout, os.Stderr)
	case "lint":
		if len(args) == 1 {
			return plan.RunWorkflowLint([]string{paths.ConfigDir()}, os.Stdout, os.Stderr)
		}
		return plan.RunWorkflowLint(args[1:], os.Stdout, os.Stderr)
	}
	helptext.Fprint(os.Stderr, workflowUsage+"\n")
	return 2
}
