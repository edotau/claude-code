package ask

import (
	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/signin"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// The terminal and sign-in UI the agent package calls but does not import.
func init() {
	agent.IsTerminal = ansi.IsTerminal
	signin.StdinIsTerminal = tui.StdinIsTerminal
	signin.LoginRow = databricks.LoginRow
	signin.PromptSecret = func(name string) (string, error) {
		return tui.Secret("Paste the value for "+name+" (input hidden, Enter when done): ", vault.Preview)
	}
}
