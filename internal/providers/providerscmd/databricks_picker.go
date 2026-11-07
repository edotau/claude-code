package providerscmd

import (
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/ui/tui"
)

func init() {
	databricks.Pick = tui.Pick
	databricks.Interactive = tui.StdinIsTerminal
}
