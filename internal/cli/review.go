package cli

import (
	"io"
	"os"

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
	"simplify":    func(a []string, _ io.Reader, o, e io.Writer) int { return source.RunSimplify(a, o, e) },
	"assumptions": plan.RunAssumptions,
	"goals":       plan.RunGoals,
	"diff":        func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunDiff(a, o, e) },
	"pr":          func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunPR(a, o, e) },
	"report":      func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunReport(a, o, e) },
	"gate":        func(a []string, _ io.Reader, o, e io.Writer) int { return diff.RunGate(a, o, e) },
}

var reviewPage = helptext.Page{
	Usage: "claude-code review <tool> [args...]",
	About: "The evaluate-code and simplify detectors as one verb; each tool keeps its former script's flags " +
		"and output.",
	Commands: [][2]string{
		{"quality", "code smells and SOLID violations in a file or tree"},
		{"complexity", "function length, nesting and branch counts"},
		{"simplify", "complexity + quality pass over the changed files"},
		{"diff", "diff lines that don't trace to the task"},
		{"pr", "a branch's changes by category, risk and review order"},
		{"report", "pr + quality findings as one review report"},
		{"gate", "advisory pre-commit summary of the staged change"},
		{"assumptions", "vague or hopeful steps in a plan"},
		{"goals", "plan steps with no verifiable goal"},
	},
	Hint: "claude-code review <tool> -h lists that tool's flags.",
}

// wantsHelp reports a leading -h/--help; a later one belongs to the tool's own parser, after its value flags.
func wantsHelp(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help")
}

func cmdReview(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		reviewPage.Fprint(os.Stdout)
		return 0
	}
	if reviewTools[args[0]] == nil {
		reviewPage.Fprint(os.Stderr)
		return 2
	}
	return reviewTools[args[0]](args[1:], os.Stdin, os.Stdout, os.Stderr)
}

var workflowPage = helptext.Page{
	Usage: "claude-code workflow <command> [args]",
	About: "Scaffold a new Workflow tool script from a pattern, or lint the saved ones.",
	Commands: [][2]string{
		{"skeleton", "print a pipeline|parallel|evaluator|orchestrator script"},
		{"lint", "check model/effort pins + meta/throw"},
	},
	Hint: "claude-code workflow <command> -h lists that command's flags.",
}

// workflowSubPage is the -h page for skeleton and lint, whose parsers have no FlagSet.
func workflowSubPage(sub string) helptext.Page {
	if sub == "lint" {
		return helptext.Page{
			Usage: "claude-code workflow lint [<root>]",
			About: "Lint <root>/workflows/*.js and <root>/skills/*/scripts/*.js for model/effort pins and " +
				"meta/throw. <root> defaults to the config dir; exits 1 when anything is flagged.",
		}
	}
	fs := helptext.NewFlagSet("skeleton", helptext.Page{})
	fs.String("name", "new-workflow", "the workflow's meta name")
	return helptext.Page{
		Usage: "claude-code workflow skeleton <pattern> [flags]",
		About: "Print a Workflow script skeleton for <pattern>: pipeline, parallel, evaluator or orchestrator.",
		Flags: fs,
	}
}

func cmdWorkflow(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		workflowPage.Fprint(os.Stdout)
		return 0
	}
	if (args[0] == "skeleton" || args[0] == "lint") && wantsHelp(args[1:]) {
		workflowSubPage(args[0]).Fprint(os.Stdout)
		return 0
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
	workflowPage.Fprint(os.Stderr)
	return 2
}
