package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/review/planlint"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var planPage = helptext.Page{
	Usage: "claude-code plan <command> [flags]",
	About: "Plan checklists (ticked/total, live claims) and plan detectors for vague steps and goals.",
	Commands: [][2]string{
		{"status", "progress per plan; --all spans every repo under the plans home"},
		{"tick", "tick named tasks on main in --range <a>..<b> (--dry-run)"},
		{"task", "tick, reopen or strike one task by hand"},
		{"assumptions", "vague or hopeful steps in a plan"},
		{"goals", "plan steps with no verifiable goal"},
	},
	Hint: "claude-code plan <command> -h lists that command's flags.",
}

// cmdPlan is the `plan` verb.
func cmdPlan(args []string) int {
	if code, ok := helpOrBare(planPage, args); ok {
		return code
	}
	switch args[0] {
	case "status":
		return cmdPlanStatus(args[1:])
	case "tick":
		return cmdPlanTick(args[1:])
	case "task":
		return cmdPlanTask(args[1:])
	case "assumptions":
		return planlint.RunAssumptions(args[1:], os.Stdin, os.Stdout, os.Stderr)
	case "goals":
		return planlint.RunGoals(args[1:], os.Stdin, os.Stdout, os.Stderr)
	default:
		return unknownSub("plan", args[0], planPage)
	}
}

func cmdPlanStatus(args []string) int {
	fs := helptext.NewFlagSet("plan status", helptext.Page{Usage: "claude-code plan status [flags]",
		About: "Show ticked/total tasks and live claims per plan of this repo."})
	all := fs.Bool("all", false, "every repo under the plans home, not just this repo's")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("", err)
	}
	summaries, err := plans.Status(plans.Dirs(*all)...)
	if err != nil {
		return helptext.Fail("plan status: %v", err)
	}
	if *format == helptext.JSON {
		if summaries == nil {
			summaries = []plans.Summary{}
		}
		if err := helptext.WriteJSON(os.Stdout, summaries); err != nil {
			return helptext.Fail("plan status: %v", err)
		}
		return 0
	}
	printPlanSummaries(summaries)
	return 0
}

func printPlanSummaries(summaries []plans.Summary) {
	if len(summaries) == 0 {
		fmt.Println("no plans")
		return
	}
	root := filepath.Join(paths.ConfigDir(), "plans")
	for _, s := range summaries {
		rel, err := filepath.Rel(root, s.Path)
		if err != nil {
			rel = s.Path
		}
		row := fmt.Sprintf("%d/%d  %s", s.Ticked, s.Total, filepath.ToSlash(rel))
		if s.Dropped > 0 {
			row = fmt.Sprintf("%d/%d (%d dropped)  %s", s.Ticked, s.Total, s.Dropped, filepath.ToSlash(rel))
		}
		fmt.Println(textutil.TruncateCells(row, 80, "…"))
		for _, c := range s.Claims {
			fmt.Printf("  %s\n", c)
		}
	}
}

func cmdPlanTick(args []string) int {
	fs := helptext.NewFlagSet("plan tick", helptext.Page{Usage: "claude-code plan tick --range <a>..<b> [flags]",
		About: "Tick tasks named by commit trailers on main; coverage alone prints candidates."})
	rng := fs.String("range", "", "commits to tick, <a>..<b> as git rev-list reads it")
	plan := fs.String("plan", "", "plan file (default: recent or claimed plans matching the range)")
	dryRun := fs.Bool("dry-run", false, "print what would tick without writing the plan")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *rng == "" {
		return helptext.Fail("plan tick: --range <a>..<b> is required")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("plan tick: %v", err)
	}
	top, _ := paths.CheckoutOf(cwd)
	if top == "" {
		return helptext.Fail("plan tick: %s is not in a git checkout", cwd)
	}
	if err := plans.TickRange(context.Background(), top, plans.WorktreeName(top), *rng, *plan, plans.MirrorDests(), *dryRun, os.Stdout); err != nil {
		return helptext.Fail("plan tick: %v", err)
	}
	return 0
}

// cmdPlanTask is `plan task`: the by-hand tick, reopen and strike for what no commit can decide.
func cmdPlanTask(args []string) int {
	fs := helptext.NewFlagSet("plan task", helptext.Page{Usage: "claude-code plan task <id> --done|--reopen|--strike <why> [flags]",
		About: "Tick, reopen or strike one task by hand; --done needs no commit trailer."})
	done := fs.Bool("done", false, "tick the task manually, with or without owns:")
	reopen := fs.Bool("reopen", false, "untick the task and drop its sha (a revert)")
	strike := fs.String("strike", "", "strike the task as superseded, with this reason")
	sha := fs.String("sha", "", "with --done: the commit to record")
	plan := fs.String("plan", "", "plan file (default: the one plan holding the id)")
	id := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if code, stop := helptext.Parse(fs, args); stop {
		return code
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	modes := 0
	for _, on := range []bool{*done, *reopen, *strike != ""} {
		if on {
			modes++
		}
	}
	if id == "" || modes != 1 || fs.NArg() > 1 || (*sha != "" && !*done) {
		return helptext.Fail("plan task: give <id> and exactly one of --done, --reopen, --strike <why>")
	}
	path, err := taskPlan(id, *plan, *reopen)
	if err != nil {
		return helptext.Fail("plan task: %v", err)
	}
	switch {
	case *done:
		err = plans.MarkDone(path, id, *sha)
	case *reopen:
		err = plans.Reopen(path, id)
	default:
		err = plans.Strike(path, id, *strike)
	}
	if err != nil {
		return helptext.Fail("plan task: %v", err)
	}
	fmt.Printf("plans: %s updated in %s\n", id, filepath.Base(path))
	return 0
}

// taskPlan is the plan file holding id: --plan as given, else the one repo plan with a matching task.
func taskPlan(id, plan string, ticked bool) (string, error) {
	if plan != "" {
		return plan, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	top, _ := paths.CheckoutOf(cwd)
	if top == "" {
		return "", fmt.Errorf("%s is not in a git checkout; pass --plan", cwd)
	}
	found := plans.TaskPlans(plans.Home(top), plans.MirrorDests(), id, func(t plans.Task) bool {
		return !t.Dropped && t.Done == ticked
	})
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no plan of this repo has a matching task %q", id)
	case 1:
		return found[0], nil
	}
	names := make([]string, len(found))
	for i, f := range found {
		names[i] = filepath.Base(f)
	}
	return "", fmt.Errorf("task %q is in %d plans (%s); pick one with --plan <file>", id, len(found), strings.Join(names, ", "))
}

// planSpec returns the plan completion tree.
func planSpec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"status": {Flags: []helptext.Flag{{Name: "all", IsBool: true},
			helptext.CompletionFormatFlag()}},
		"assumptions": {Flags: []helptext.Flag{helptext.BoolFlag("json")}}, "goals": {Flags: []helptext.Flag{helptext.BoolFlag("json")}},
		"tick": {Flags: []helptext.Flag{{Name: "range"}, {Name: "plan"}, {Name: "dry-run", IsBool: true}}},
		"task": {Flags: []helptext.Flag{helptext.BoolFlag("done"), helptext.BoolFlag("reopen"), {Name: "strike"}, {Name: "sha"}, {Name: "plan"}}},
	}}
}
