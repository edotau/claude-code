package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/setup"
)

const setupWorkspaceUsage = `usage: claude-code setup workspace init [<root>] [--with agents,commands,skills,settings,memory] [--dry-run]
       claude-code setup workspace link <agents|commands|skills> <name> [<root>]
       claude-code setup workspace status [<root>]
<root> defaults to the current directory and must be a git toplevel outside ~/.claude.`

// cmdSetupWorkspace is the `setup workspace` leg: a project .claude/ scaffold, link and status.
func cmdSetupWorkspace(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, setupWorkspaceUsage)
		return 2
	}
	fs := flag.NewFlagSet("setup workspace "+args[0], flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	with := fs.String("with", "", "comma-separated subset of agents,commands,skills,settings,memory (default all)")
	dryRun := fs.Bool("dry-run", false, "print the plan, write nothing")
	pos, err := outfmt.Parse(fs, args[1:])
	if err != nil {
		return 2
	}
	switch args[0] {
	case "init":
		return setupWorkspaceInit(pos, *with, *dryRun)
	case "link":
		if len(pos) < 2 || len(pos) > 3 {
			fmt.Fprintln(os.Stderr, setupWorkspaceUsage)
			return 2
		}
		return setupWorkspaceErr(setup.WorkspaceLink(setupWorkspaceRoot(pos[2:]), pos[0], pos[1], os.Stdout))
	case "status":
		return setupWorkspaceErr(setup.WorkspaceStatus(setupWorkspaceRoot(pos), os.Stdout))
	}
	fmt.Fprintln(os.Stderr, setupWorkspaceUsage)
	return 2
}

// setupWorkspaceInit runs init on the optional positional root.
func setupWorkspaceInit(pos []string, with string, dryRun bool) int {
	if len(pos) > 1 {
		fmt.Fprintln(os.Stderr, setupWorkspaceUsage)
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
