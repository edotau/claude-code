package agent

import (
	"fmt"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/router"
)

// Configured is the no-network presence check: the vendor binary, plus an opencode login for the cell's provider.
func Configured(runner string, c router.Cell) error {
	s, ok := LookupSchema(runner)
	if !ok {
		return fmt.Errorf("unknown runner %s", runner)
	}
	if !s.External() {
		return nil
	}
	if _, err := paths.VendorBin(s.CLI.Binary); err != nil {
		return err
	}
	return s.eng().presence(c)
}
