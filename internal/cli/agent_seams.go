package cli

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// The terminal and sign-in UI the agent package calls but does not import.
func init() {
	agent.IsTerminal = ansi.IsTerminal
	agent.StdinIsTerminal = tui.StdinIsTerminal
	agent.LoginRow = databricks.LoginRow
	agent.PromptSecret = func(name string) (string, error) {
		return tui.Secret("Paste the value for "+name+" (input hidden, Enter when done): ", secrets.Preview)
	}
}
