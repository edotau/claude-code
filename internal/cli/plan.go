package cli

import (
	"fmt"
	"os"
	"path/filepath"

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
