package reviewcmd

import (
	"io"
	"os"
	"slices"

	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/diff"
	"github.com/edotau/claude-code/internal/review/planlint"
	"github.com/edotau/claude-code/internal/review/source"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// reviewTools are the evaluate-code/simplify detectors, each keeping its former Python CLI.
var reviewTools = map[string]func(args []string, stdin io.Reader, stdout, stderr io.Writer) int{
	"quality":     func(a []string, _ io.Reader, o, e io.Writer) int { return source.RunQuality(a, o, e) },
	"complexity":  func(a []string, _ io.Reader, o, e io.Writer) int { return source.RunComplexity(a, o, e) },
	"simplify":    func(a []string, _ io.Reader, o, e io.Writer) int { return source.RunSimplify(a, o, e) },
	"assumptions": planlint.RunAssumptions,
	"goals":       planlint.RunGoals,
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

// Review dispatches `review <tool>`.
func Review(args []string) int {
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

// Workflow dispatches `workflow skeleton|lint`.
func Workflow(args []string) int {
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
		return planlint.RunSkeleton(args[1:], os.Stdout, os.Stderr)
	case "lint":
		return workflowLint(args[1:])
	}
	workflowPage.Fprint(os.Stderr)
	return 2
}

func workflowLint(args []string) int {
	if len(args) == 0 {
		return planlint.RunWorkflowLint([]string{paths.ConfigDir()}, os.Stdout, os.Stderr)
	}
	return planlint.RunWorkflowLint(args, os.Stdout, os.Stderr)
}

// ToolNames lists the review detectors, derived from reviewTools so a new one is completable for free.
func ToolNames() []string {
	out := make([]string, 0, len(reviewTools))
	for n := range reviewTools {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// ReviewSpec is the completion node for `review`: every detector, none with completable flags.
func ReviewSpec() *completion.Node {
	sub := map[string]*completion.Node{}
	for _, n := range ToolNames() {
		sub[n] = &completion.Node{}
	}
	return &completion.Node{Sub: sub}
}

// WorkflowPatterns are the `workflow skeleton` pattern names.
func WorkflowPatterns() []string {
	return []string{"pipeline", "parallel", "evaluator", "orchestrator"}
}

// WorkflowSpec is the completion node for `workflow`.
func WorkflowSpec() *completion.Node {
	return &completion.Node{Sub: map[string]*completion.Node{
		"skeleton": {Args: []func() []string{WorkflowPatterns}, Flags: []completion.Flag{{Name: "name"}}},
		"lint":     {},
	}}
}
