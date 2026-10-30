package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/setup"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var setupWorkspacePage = helptext.Page{
	Usage: "claude-code install --setup workspace <op> [<root>]",
	About: "Scope a project's own .claude/: scaffold it, link a harness item into it, or report it. " +
		"<root> defaults to cwd and must be a git toplevel outside ~/.claude.",
	Commands: [][2]string{
		{"init", "scaffold .claude/ (--with LIST, --dry-run)"},
		{"link", "link one of agents|commands|skills into it"},
		{"status", "report what the project .claude/ holds"},
	},
}

// setupWorkspaceOps is each op's own -h page.
var setupWorkspaceOps = map[string]helptext.Page{
	"init": {Usage: "claude-code install --setup workspace init [<root>] [flags]",
		About: "Scaffold the project's .claude/."},
	"link": {Usage: "claude-code install --setup workspace link <kind> <name> [<root>]",
		About: "Link one harness item into the project's .claude/; <kind> is agents, commands or skills."},
	"status": {Usage: "claude-code install --setup workspace status [<root>]",
		About: "Report what the project's .claude/ holds."},
}

// cmdSetupWorkspace is the `setup workspace` leg: a project .claude/ scaffold, link and status.
func cmdSetupWorkspace(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		setupWorkspacePage.Fprint(os.Stderr)
		return 0
	}
	op, ok := setupWorkspaceOps[args[0]]
	if !ok {
		setupWorkspacePage.Fprint(os.Stderr)
		return 2
	}
	fs := helptext.NewFlagSet("setup workspace "+args[0], op)
	with := fs.String("with", "", "subset of agents,commands,skills,settings,memory (default all)")
	dryRun := fs.Bool("dry-run", false, "print the plan, write nothing")
	pos, err := helptext.ParseArgs(fs, args[1:])
	if err != nil {
		return helptext.ExitCode(err)
	}
	if args[0] != "init" && (*with != "" || *dryRun) { // link would write despite --dry-run; refuse
		fmt.Fprintf(os.Stderr, "claude-code: setup workspace %s: --with/--dry-run apply to init only\n", args[0])
		return 2
	}
	switch args[0] {
	case "init":
		return setupWorkspaceInit(pos, *with, *dryRun)
	case "link":
		if len(pos) < 2 || len(pos) > 3 {
			setupWorkspacePage.Fprint(os.Stderr)
			return 2
		}
		return setupWorkspaceErr(setup.WorkspaceLink(setupWorkspaceRoot(pos[2:]), pos[0], pos[1], os.Stdout))
	case "status":
		return setupWorkspaceErr(setup.WorkspaceStatus(setupWorkspaceRoot(pos), os.Stdout))
	}
	setupWorkspacePage.Fprint(os.Stderr)
	return 2
}

// setupWorkspaceInit runs init on the optional positional root.
func setupWorkspaceInit(pos []string, with string, dryRun bool) int {
	if len(pos) > 1 {
		setupWorkspacePage.Fprint(os.Stderr)
		return 2
	}
	var parts []string
	if with != "" {
		parts = strings.Split(with, ",")
	}
	return setupWorkspaceErr(setup.WorkspaceInit(setup.WorkspaceInitOpts{
		Root: setupWorkspaceRoot(pos), With: parts, DryRun: dryRun, Out: os.Stdout,
	}))
}

// setupWorkspaceRoot is the optional trailing positional, else the current directory.
func setupWorkspaceRoot(pos []string) string {
	if len(pos) > 0 && pos[len(pos)-1] != "" {
		return pos[len(pos)-1]
	}
	return "."
}

// setupWorkspaceErr maps an error to exit 1 with a prefixed message; nil is 0.
func setupWorkspaceErr(err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(os.Stderr, "claude-code: setup workspace:", err)
	if errors.Is(err, setup.ErrWorkspaceUsage) {
		return 2
	}
	return 1
}
