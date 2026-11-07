package cli

import "github.com/edotau/claude-code/internal/ui/menu"

// configureAgentNames are the agent CLIs `configure --agent` can scope to: the agents plus claude itself.
func configureAgentNames() []string { return append([]string{"claude"}, cliAgentNames()...) }

// configureSections derives from menu.SectionNames, the package's own run order.
func configureSections() []string {
	return append(append([]string{}, menu.SectionNames...), "desktop")
}
