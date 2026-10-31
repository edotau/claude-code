// setup is one verb for the three setup modes: adopt an existing ~/.claude, nuke it for a fresh
// start, or scope a project's own .claude/; workspace is cmdSetupWorkspace below.
package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/setup"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var setupPage = helptext.Page{
	Usage: "claude-code install --setup <mode> [flags]",
	About: "Move a harness checkout in or out: adopt a staged clone, remove one, or scope a project's own .claude/.",
	Commands: [][2]string{
		{"adopt", "adopt a staged clone into an existing ~/.claude; --from DIR"},
		{"nuke", "remove the harness directory (dry-run without --apply)"},
		{"workspace", "scaffold, link into, or report a project .claude/"},
	},
	Hint: "claude-code install --setup <mode> -h lists that mode's flags.",
}

func cmdSetup(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		setupPage.Fprint(os.Stderr)
		return 0
	}
	switch args[0] {
	case "adopt":
		return cmdSetupAdopt(args[1:])
	case "nuke":
		return cmdSetupNuke(args[1:])
	case "workspace":
		return cmdSetupWorkspace(args[1:])
	}
	return fail("setup: unknown mode %q (adopt|nuke|workspace)", args[0])
}

func cmdSetupAdopt(args []string) int {
	fs := helptext.NewFlagSet("setup adopt", helptext.Page{
		Usage: "claude-code install --setup adopt --from DIR [flags]",
		About: "Adopt a staged clone into an existing ~/.claude, backing up what it replaces.",
	})
	from := fs.String("from", "", "the staged clone to adopt from")
	into := fs.String("into", paths.ConfigDir(), "existing ~/.claude to adopt into")
	dryRun := fs.Bool("dry-run", false, "report clashes and stop")
	label := fs.String("label", "adopt", "backup directory label")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *from == "" {
		fmt.Fprintln(os.Stderr, "claude-code: setup adopt: --from is required")
		return 2
	}
	err := setup.Adopt(setup.AdoptOpts{From: *from, Into: *into, Label: *label, DryRun: *dryRun, Out: os.Stdout})
	return setupExit(err, "setup adopt")
}

func cmdSetupNuke(args []string) int {
	fs := helptext.NewFlagSet("setup nuke", helptext.Page{
		Usage: "claude-code install --setup nuke [flags]",
		About: "Remove the harness directory for a fresh start; a dry run unless --apply, " +
			"which asks you to type the path to confirm.",
	})
	dir := fs.String("dir", paths.ConfigDir(), "the harness directory to remove")
	apply := fs.Bool("apply", false, "actually remove it (dry-run without this flag)")
	keepMemory := fs.Bool("keep-memory", false, "archive memory/, plans/, and any old memory dirs before removing")
	account := fs.Bool("account", false, "report what removing the Claude Code account state would take")
	removeCLI := fs.Bool("cli", false, "report what removing the Claude Code CLI install would take")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *account {
		fmt.Printf("setup nuke --account is informational only; would remove %s\nreinstall: log in again with `claude-code configure`\n", paths.ClaudeJSON())
	}
	if *removeCLI {
		fmt.Println("setup nuke --cli is informational only; would remove ~/.local/bin/claude")
		fmt.Println("reinstall: https://docs.claude.com/en/docs/claude-code/quickstart")
	}
	confirm := ""
	if *apply {
		fmt.Print("type the directory path to confirm: ")
		if _, err := fmt.Scanln(&confirm); err != nil {
			return setupExit(fmt.Errorf("%w: %v", setup.ErrConfirmMismatch, err), "setup nuke")
		}
	}
	err := setup.Nuke(setup.NukeOpts{Dir: *dir, Apply: *apply, Confirm: confirm, KeepMemory: *keepMemory, Out: os.Stdout})
	return setupExit(err, "setup nuke")
}

// setupExit maps a refusal (bad precondition, wrong confirmation) to exit 2, any other error to 1.
func setupExit(err error, verb string) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, setup.ErrAlreadyCheckout), errors.Is(err, setup.ErrNotAClone), errors.Is(err, setup.ErrConfirmMismatch), errors.Is(err, setup.ErrNotConfigDir), errors.Is(err, setup.ErrWorkspaceUsage):
		fmt.Fprintf(os.Stderr, "claude-code: %s: %v\n", verb, err)
		return 2
	default:
		return fail("%s: %v", verb, err)
	}
}

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
		return setupExit(setup.WorkspaceLink(setupWorkspaceRoot(pos[2:]), pos[0], pos[1], os.Stdout), "setup workspace")
	case "status":
		return setupExit(setup.WorkspaceStatus(setupWorkspaceRoot(pos), os.Stdout), "setup workspace")
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
	return setupExit(setup.WorkspaceInit(setup.WorkspaceInitOpts{
		Root: setupWorkspaceRoot(pos), With: parts, DryRun: dryRun, Out: os.Stdout,
	}), "setup workspace")
}

// setupWorkspaceRoot is the optional trailing positional, else the current directory.
func setupWorkspaceRoot(pos []string) string {
	if len(pos) > 0 && pos[len(pos)-1] != "" {
		return pos[len(pos)-1]
	}
	return "."
}
