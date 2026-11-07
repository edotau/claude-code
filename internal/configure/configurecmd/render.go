package configurecmd

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// configureRender runs --restore / --revert; the agent flag is a comma list there.
func configureRender(restore, revert bool, section, agents, out string, check, dryRun, renderAgents, noAgents bool) int {
	if err := renderModeConflict(restore, revert, section); err != nil {
		return helptext.Fail("%v", err)
	}
	if revert {
		return configureRevert(agents, dryRun, check || out != "" || renderAgents || noAgents)
	}
	if check || dryRun {
		return helptext.Fail("configure: --restore takes no --check or --dry-run")
	}
	return configureRestore(agents, out, renderAgents, noAgents)
}

func configureRestore(agents, out string, renderAgents, noAgents bool) int {
	if err := unknownAgent(agents); err != nil {
		return helptext.Fail("%v", err)
	}
	if noAgents && (renderAgents || agents != "") {
		return helptext.Fail("configure: --no-agents excludes --agents and --agent")
	}
	if out != "" && (renderAgents || agents != "") {
		return helptext.Fail("configure: --out resets a settings file; --agents and --agent never write one")
	}
	return cmdRestore(agents, out, renderAgents, noAgents)
}

// configureRevert runs --revert; extras is true when a flag that only --restore takes was given.
func configureRevert(agents string, dryRun, extras bool) int {
	if extras {
		return helptext.Fail("configure: --revert takes only --agent and --dry-run")
	}
	if err := unknownAgent(agents); err != nil {
		return helptext.Fail("%v", err)
	}
	return cmdRevert(agents, dryRun)
}

func renderModeConflict(restore, revert bool, section string) error {
	switch {
	case restore && revert:
		return errors.New("configure: --restore and --revert are mutually exclusive")
	case section != "":
		return fmt.Errorf("configure: --restore/--revert take no page (got %q)", section)
	}
	return nil
}

// unknownAgent rejects a typo the engine would skip silently.
func unknownAgent(agents string) error {
	for a := range strings.SplitSeq(agents, ",") {
		if a = strings.TrimSpace(a); a != "" && !slices.Contains(agent.DefaultAgents, a) {
			return fmt.Errorf("configure: unknown --agent %q (%s)", a, strings.Join(agent.DefaultAgents, "|"))
		}
	}
	return nil
}

// cmdRestore is `configure --restore`; the work lives in launch.Restore.
func cmdRestore(agents, out string, forceHomes, noAgents bool) int {
	if err := launch.Restore(os.Stdout, agents, out, forceHomes, noAgents); err != nil {
		return helptext.Fail("%v", err)
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
