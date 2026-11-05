package cli

import (
	"context"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// cmdRestore is `configure --restore`; the work lives in launch.Restore.
func cmdRestore(agents, out string, check, dryRun, forceHomes, noAgents bool) int {
	drift, err := launch.Restore(os.Stdout, agents, out, check, dryRun, forceHomes, noAgents)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if drift {
		return 1
	}
	return 0
}

// cmdRevert handles `configure --revert [--agent a,b] [--dry-run]`.
func cmdRevert(agents string, dryRun bool) int {
	if err := launch.Revert(os.Stdout, agents, dryRun); err != nil {
		return helptext.Fail("configure --revert: %v", err)
	}
	return 0
}

// toolsFlag is --tools, bare (every tool) or --tools=name,name2 (that subset only).
type toolsFlag struct {
	set   bool
	names []string
}

func (t *toolsFlag) String() string {
	if t == nil || !t.set {
		return ""
	}
	return strings.Join(t.names, ",")
}

func (t *toolsFlag) Set(s string) error {
	t.set = true
	if s != "" && s != "true" {
		t.names = strings.Split(s, ",")
	}
	return nil
}

func (t *toolsFlag) IsBoolFlag() bool { return true } // allows the bare "--tools" form

func cmdInstall(args []string) int {
	if len(args) > 0 && (args[0] == "--setup" || args[0] == "-setup") {
		return cmdSetup(args[1:])
	}
	fs := helptext.NewFlagSet("install", helptext.Page{
		Usage: "claude-code install [flags]\nclaude-code install --setup <mode> [flags]",
		About: "Build-time setup: install the Claude Code CLI and agent links, then render settings.json. " +
			"--tools installs only the agent CLIs; --setup migrates, wipes or scopes a .claude/ tree.",
		Hint: "claude-code install --setup -h lists the setup modes.",
	})
	dry := fs.Bool("dry-run", false, "print what would change; write nothing")
	verbose := fs.Bool("verbose", false, "print every step in full, instead of one line each")
	var tf toolsFlag
	fs.Var(&tf, "tools", "install CLIs only: bare = all; --tools=name,... selects tools")
	setup := fs.String("setup", "", "adopt|nuke|workspace: migrate, wipe, or scope .claude/ (first arg)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *setup != "" {
		return helptext.Fail("install: --setup must come first: claude-code install --setup <mode> [flags]")
	}
	if tf.set {
		if err := tools.Install(context.Background(), os.Stdout, tf.names, *dry); err != nil {
			return helptext.Fail("install --tools: %v", err)
		}
		return 0
	}
	if err := launch.Install(os.Stdout, *dry, *verbose); err != nil {
		return helptext.Fail("install: %v", err)
	}
	return 0
}
