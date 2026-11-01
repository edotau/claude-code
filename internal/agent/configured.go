package agent

import (
	"fmt"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
)

// Configured is the no-network presence check: the agent binary, plus an opencode login for the cell's provider.
func Configured(agent string, c router.Cell) error {
	s, ok := LookupSchema(agent)
	if !ok {
		return fmt.Errorf("unknown agent %s", agent)
	}
	if !s.External() {
		return nil
	}
	if _, err := paths.AgentBin(s.CLI.Binary); err != nil {
		return err
	}
	return s.eng().presence(c)
}
