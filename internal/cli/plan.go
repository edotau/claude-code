package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var planPage = helptext.Page{
	Usage: "claude-code plan <command> [flags]",
	About: "Plan checklists: ticked/total per plan, live task claims and partly-landed notes.",
	Commands: [][2]string{
		{"status", "progress per plan; --all spans every repo under the plans home"},
		{"tick", "tick tasks covered by each commit in --range <a>..<b> (--dry-run)"},
	},
	Hint: "claude-code plan status -h lists its flags.",
}

func cmdPlan(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		planPage.Fprint(os.Stderr)
		return 0
	}
	switch args[0] {
	case "status":
		return cmdPlanStatus(args[1:])
	case "tick":
		return cmdPlanTick(args[1:])
	default:
		fail("plan: unknown subcommand %q", args[0])
		planPage.Fprint(os.Stderr)
		return 2
	}
}

func cmdPlanStatus(args []string) int {
	fs := helptext.NewFlagSet("plan status", helptext.Page{Usage: "claude-code plan status [flags]"})
	all := fs.Bool("all", false, "every repo under the plans home, not just this repo's")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat("", err)
	}
	summaries, err := plans.Status(plans.Dirs(*all)...)
	if err != nil {
		return fail("plan status: %v", err)
	}
	if *format == helptext.JSON {
		if summaries == nil {
			summaries = []plans.Summary{}
		}
		if err := helptext.WriteJSON(os.Stdout, summaries); err != nil {
			return fail("plan status: %v", err)
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
	fs := helptext.NewFlagSet("plan tick", helptext.Page{Usage: "claude-code plan tick --range <a>..<b> [flags]"})
	rng := fs.String("range", "", "commits to tick, <a>..<b> as git rev-list reads it")
	plan := fs.String("plan", "", "plan file to tick (default: this repo's active plan)")
	dryRun := fs.Bool("dry-run", false, "print what would tick without writing the plan")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *rng == "" {
		return fail("plan tick: --range <a>..<b> is required")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail("plan tick: %v", err)
	}
	top, _ := paths.CheckoutOf(cwd)
	if top == "" {
		return fail("plan tick: %s is not in a git checkout", cwd)
	}
	if err := plans.TickRange(context.Background(), top, plans.WorktreeName(top), *rng, *plan, hooks.MirrorDests(), *dryRun, os.Stdout); err != nil {
		return fail("plan tick: %v", err)
	}
	return 0
}
