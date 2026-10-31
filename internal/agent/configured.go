package agent

import (
	"fmt"
	"slices"
	"strings"

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
	if u := copilotPinLogin(); runner == "copilot" && u != "" && !slices.Contains(GHAccounts(), u) {
		return fmt.Errorf("pinned gh user %s is not logged in (gh auth login); no other account is used", u)
	}
	if runner == "opencode" {
		if id, _, found := strings.Cut(c.Model, "/"); found {
			_, err := opencodeCredential(id)
			return err
		}
	}
	return nil
}
