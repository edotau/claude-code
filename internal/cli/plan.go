package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
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
	format := outfmt.Flag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("%v", err)
	}
	summaries, err := plans.Status(planDirs(*all)...)
	if err != nil {
		return fail("plan status: %v", err)
	}
	if *format == outfmt.JSON {
		if summaries == nil {
			summaries = []plans.Summary{}
		}
		if err := outfmt.Write(os.Stdout, summaries); err != nil {
			return fail("plan status: %v", err)
		}
		return 0
	}
	if len(summaries) == 0 {
		fmt.Println("no plans")
		return 0
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
	return 0
}

// planDirs is the repo's Home by default, else the plans home plus every direct repo subdir.
func planDirs(all bool) []string {
	root := filepath.Join(paths.ConfigDir(), "plans")
	if !all {
		cwd, err := os.Getwd()
		if err != nil {
			return []string{root}
		}
		top, _ := paths.CheckoutOf(cwd)
		if top == "" {
			top = cwd
		}
		return []string{plans.Home(top)}
	}
	dirs := []string{root}
	entries, err := os.ReadDir(root)
	if err != nil {
		return dirs
	}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs
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
	worktree := "" // a linked worktree's name, matched against task claims
	if main := paths.MainCheckout(top); main != top {
		worktree = filepath.Base(top)
	}
	if err := tickRange(context.Background(), top, worktree, *rng, *plan, *dryRun, os.Stdout); err != nil {
		return fail("plan tick: %v", err)
	}
	return 0
}

// tickRange ticks plan (default: dir's active plan, preferring worktree's claim) for each commit in rng.
func tickRange(ctx context.Context, dir, worktree, rng, plan string, dryRun bool, w io.Writer) error {
	if plan == "" {
		var ok bool
		if plan, ok = plans.Active(plans.Home(paths.MainCheckout(dir)), worktree, nil); !ok {
			fmt.Fprintln(w, "plans: no active plan to tick")
			return nil
		}
	}
	commits, err := plans.RangeCommits(ctx, dir, rng)
	if err != nil {
		return err
	}
	results, err := plans.TickCommits(plan, commits, dryRun)
	if err != nil {
		return err
	}
	verb, base := "ticked", strings.TrimSuffix(filepath.Base(plan), ".md")
	if dryRun {
		verb = "would tick"
	}
	for _, r := range results {
		if len(r.Ticked) > 0 {
			fmt.Fprintf(w, "plans: %s %s (%s) in %s\n", verb, strings.Join(r.Ticked, "; "), r.Sha, base)
		}
		if len(r.Partial) > 0 {
			fmt.Fprintf(w, "plans: partly landed %s (%s) in %s\n", strings.Join(r.Partial, "; "), r.Sha, base)
		}
	}
	if len(results) == 0 {
		fmt.Fprintf(w, "plans: %d commits, nothing to tick in %s\n", len(commits), base)
	}
	return nil
}
