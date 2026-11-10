package ask

import (
	"github.com/edotau/claude-code/internal/agent/launchplan"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// The terminal and sign-in UI the agent package calls but does not import.
func init() {
	launchplan.IsTerminal = ansi.IsTerminal
	configure.StdinIsTerminal = tui.StdinIsTerminal
	configure.LoginRow = databricks.LoginRow
	configure.PromptSecret = func(name string) (string, error) {
		return tui.Secret("Paste the value for "+name+" (input hidden, Enter when done): ", vault.Preview)
	}
}
