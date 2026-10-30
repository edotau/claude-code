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
	if runner == "claude" {
		return nil
	}
	spec, ok := LookupSpec(runner)
	if !ok {
		return fmt.Errorf("unknown runner %s", runner)
	}
	if _, err := paths.VendorBin(spec.Binary); err != nil {
		return err
	}
	if u := CopilotPinLogin(); runner == "copilot" && u != "" && !slices.Contains(GHAccounts(), u) {
		return fmt.Errorf("pinned gh user %s is not logged in (gh auth login); no other account is used", u)
	}
	if runner == "opencode" {
		if id, _, found := strings.Cut(c.Model, "/"); found {
			_, err := OpencodeCredential(id)
			return err
		}
	}
	return nil
}
