package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

const menuWidth = 80

var planPage = helptext.Page{
	Usage: "claude-code plan <command> [flags]",
	About: "Plan checklists: ticked/total per plan and live task claims.",
	Commands: [][2]string{
		{"status", "progress per plan; --all spans every repo under the plans home"},
		{"tick", "tick tasks covered by each commit in --range <a>..<b> (--dry-run)"},
	},
	Hint: "claude-code plan <command> -h lists that command's flags.",
}

// cmdPlan is the `plan` verb.
func cmdPlan(args []string) int {
	if len(args) == 0 || isHelpVerb(args[0]) {
		planPage.Fprint(os.Stderr)
		return 0
	}
	switch args[0] {
	case "status":
		return cmdPlanStatus(args[1:])
	case "tick":
		return cmdPlanTick(args[1:])
	default:
		helptext.Fail("plan: unknown subcommand %q", args[0])
		planPage.Fprint(os.Stderr)
		return 2
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
		fmt.Println(textutil.Truncate(row, menuWidth))
		for _, c := range s.Claims {
			fmt.Printf("  %s\n", c)
		}
	}
}

func cmdPlanTick(args []string) int {
	fs := helptext.NewFlagSet("plan tick", helptext.Page{Usage: "claude-code plan tick --range <a>..<b> [flags]",
		About: "Tick the plan tasks that each commit in --range landed."})
	rng := fs.String("range", "", "commits to tick, <a>..<b> as git rev-list reads it")
	plan := fs.String("plan", "", "plan file to tick (default: the plan whose open tasks own the range's files)")
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

// planSpec returns the plan completion tree.
func planSpec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"status": {Flags: []helptext.Flag{{Name: "all", IsBool: true},
			helptext.CompletionFormatFlag()}},
		"tick": {Flags: []helptext.Flag{{Name: "range"}, {Name: "plan"}, {Name: "dry-run", IsBool: true}}},
	}}
}
