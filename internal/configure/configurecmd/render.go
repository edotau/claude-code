package configurecmd

import (
	"errors"
	"fmt"
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
	return launch.CmdRestore(agents, out, renderAgents, noAgents)
}

// configureRevert runs --revert; extras is true when a flag that only --restore takes was given.
func configureRevert(agents string, dryRun, extras bool) int {
	if extras {
		return helptext.Fail("configure: --revert takes only --agent and --dry-run")
	}
	if err := unknownAgent(agents); err != nil {
		return helptext.Fail("%v", err)
	}
	return launch.CmdRevert(agents, dryRun)
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
