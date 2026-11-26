package workflows

import (
	"fmt"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
)

const skeletonUsage = "usage: claude-code workflow skeleton [-h] [--name NAME]\n" +
	"                                     {evaluator,orchestrator,parallel,pipeline}\n"

var skeletonPatterns = map[string]func(string) string{
	"pipeline":     assets.WorkflowPipelineSkeleton,
	"parallel":     assets.WorkflowParallelSkeleton,
	"evaluator":    assets.WorkflowEvaluatorSkeleton,
	"orchestrator": assets.WorkflowOrchestratorSkeleton,
}

// skeletonArgs is the parsed command line; code/done report an early exit (usage error).
type skeletonArgs struct {
	name                      string
	positionals, unrecognized []string
	code                      int
	done                      bool
}

func parseSkeletonArgs(args []string, stderr io.Writer) skeletonArgs {
	p := skeletonArgs{name: "new-workflow"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name":
			i++
			if i >= len(args) {
				fmt.Fprintln(stderr, "workflow skeleton: error: argument --name: expected one argument")
				return skeletonArgs{code: 2, done: true}
			}
			p.name = args[i]
		case strings.HasPrefix(a, "--name="):
			p.name = strings.TrimPrefix(a, "--name=")
		case strings.HasPrefix(a, "-") && a != "-":
			p.unrecognized = append(p.unrecognized, a)
		case len(p.positionals) == 1:
			p.unrecognized = append(p.unrecognized, a)
		default:
			p.positionals = append(p.positionals, a)
		}
	}
	return p
}

// RunSkeleton emits a JavaScript workflow skeleton for the selected pattern.
func RunSkeleton(args []string, stdout, stderr io.Writer) int {
	parsed := parseSkeletonArgs(args, stderr)
	if parsed.done {
		return parsed.code
	}
	name, positionals, unrecognized := parsed.name, parsed.positionals, parsed.unrecognized
	if len(unrecognized) > 0 {
		helptext.Fprint(stderr, skeletonUsage)
		fmt.Fprintf(stderr, "workflow skeleton: error: unrecognized arguments: %s\n", strings.Join(unrecognized, " "))
		return 2
	}

	if len(positionals) == 0 {
		helptext.Fprint(stderr, skeletonUsage)
		fmt.Fprintln(stderr, "workflow skeleton: error: the following arguments are required: pattern")
		return 2
	}

	pattern := positionals[0]
	fn, ok := skeletonPatterns[pattern]
	if !ok {
		helptext.Fprint(stderr, skeletonUsage)
		fmt.Fprintf(stderr, "workflow skeleton: error: argument pattern: invalid choice: %q (choose from 'evaluator', 'orchestrator', 'parallel', 'pipeline')\n", pattern)
		return 2
	}

	if _, err := fmt.Fprintln(stdout, fn(name)); err != nil {
		fmt.Fprintf(stderr, "workflow skeleton: error: writing skeleton: %v\n", err)
		return 1
	}
	if _, err := io.WriteString(stdout, assets.WorkflowSkeletonNote()+"\n"); err != nil {
		fmt.Fprintf(stderr, "workflow skeleton: error: writing note: %v\n", err)
		return 1
	}
	return 0
}
