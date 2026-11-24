package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/review/bench"
	"github.com/edotau/claude-code/internal/review/diff"
	"github.com/edotau/claude-code/internal/review/source"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/workflows"
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

var workflowPage = helptext.Page{
	Usage: "claude-code workflow <command> [args]",
	About: "Scaffold a new Workflow tool script from a pattern, or lint the saved ones.",
	Commands: [][2]string{
		{"skeleton", "print a pipeline|parallel|evaluator|orchestrator script"},
		{"lint", "check pins, meta/throw, syntax and the saved-workflows table"},
		{"sync", "write embedded helper blocks into saved scripts"},
	},
	Hint: "claude-code workflow <command> -h lists that command's flags.",
}

// workflowSubPage is the -h page for skeleton, lint and sync, whose parsers have no FlagSet.
func workflowSubPage(sub string) helptext.Page {
	if sub == "sync" {
		return helptext.Page{
			Usage: "claude-code workflow sync [<root>]",
			About: "Write embedded helper blocks into marked scripts; " +
				"<root> defaults to this checkout, else the config dir. " +
				"Exits: 0 synced or unchanged; 1 file/marker error; 2 usage.",
		}
	}
	if sub == "lint" {
		return helptext.Page{
			Usage: "claude-code workflow lint [<root>]",
			About: "Lint <root>/workflows/*.js and <root>/skills/*/scripts/*.js for model/effort pins and " +
				"meta/throw; compile each with node and check helper and saved-workflows table drift. " +
				"<root> defaults to this checkout, else the config dir; exits 1 when anything is flagged.",
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

// cmdWorkflow dispatches `workflow skeleton|lint|sync`.
func cmdWorkflow(args []string) int {
	if len(args) == 0 || isHelpVerb(args[0]) {
		workflowPage.Fprint(os.Stdout)
		return 0
	}
	// only a leading -h: a later one belongs to the tool's own parser, after its value flags
	if workflowWantsHelp(args) {
		workflowSubPage(args[0]).Fprint(os.Stdout)
		return 0
	}
	switch args[0] {
	case "skeleton":
		return workflows.RunSkeleton(args[1:], os.Stdout, os.Stderr)
	case "lint":
		return runWorkflowVerb("lint", args[1:], workflows.RunWorkflowLint)
	case "sync":
		return runWorkflowVerb("sync", args[1:], workflows.RunWorkflowSync)
	}
	return unknownSub("workflow", args[0], workflowPage)
}

// workflowWantsHelp: skeleton takes -h after its pattern, lint and sync only as their sole argument.
func workflowWantsHelp(args []string) bool {
	if !slices.Contains([]string{"skeleton", "lint", "sync"}, args[0]) || len(args) < 2 || (args[0] != "skeleton" && len(args) > 2) {
		return false
	}
	return helptext.WantsHelp(helpArgs(args))
}

// helpArgs is where a workflow sub-verb's -h may sit: skeleton takes it after its pattern, lint only first.
func helpArgs(args []string) []string {
	if args[0] == "skeleton" {
		return args[1:]
	}
	return args[1:2]
}

func runWorkflowVerb(verb string, args []string, run func([]string, io.Writer, io.Writer) int) int {
	target, err := workflowTarget(verb, args, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return run(target, os.Stdout, os.Stderr)
}

// workflowTarget picks the root (argument, else enclosing harness checkout, else config dir) and prints it first.
func workflowTarget(verb string, args []string, out io.Writer) ([]string, error) {
	if len(args) > 1 || (len(args) == 1 && (args[0] == "" || strings.HasPrefix(args[0], "-"))) {
		return nil, fmt.Errorf("usage: claude-code workflow %s [<root>]", verb)
	}
	if len(args) == 0 {
		args = []string{paths.ConfigDir()}
		if wd, err := os.Getwd(); err == nil {
			if top, _ := paths.CheckoutOf(wd); isHarnessCheckout(top) {
				args = []string{top}
			}
		}
	}
	shown, err := filepath.Abs(args[0])
	if err != nil {
		shown = args[0]
	}
	fmt.Fprintf(out, "workflow %s: %s\n", verb, shown)
	return args, nil
}

// isHarnessCheckout reports whether top holds the saved workflows and their embedded blocks.
func isHarnessCheckout(top string) bool {
	if top == "" {
		return false
	}
	for _, d := range []string{"workflows", "internal/assets/files/workflows/blocks"} {
		if fi, err := os.Stat(filepath.Join(top, d)); err != nil || !fi.IsDir() {
			return false
		}
	}
	return true
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

// workflowPatterns are the `workflow skeleton` pattern names.
func workflowPatterns() []string {
	return []string{"pipeline", "parallel", "evaluator", "orchestrator"}
}

// workflowSpec is the completion node for `workflow`.
func workflowSpec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"skeleton": {Args: []func() []string{workflowPatterns}, Flags: []helptext.Flag{{Name: "name"}}},
		"lint":     {},
		"sync":     {},
	}}
}
