// settings.go — settings saves behind the menu pages (routing, codex) and routing input validation.
package configure

import (
	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
)

// ValidateRoutingInput runs f.Set against a scratch Routing, so a bad value is reported before staging.
func ValidateRoutingInput(f settings.RoutingField, input string) error {
	var scratch providers.Routing
	return f.Set(&scratch, input)
}

// SaveRouting applies staged values and resets to the overlay routing object in one transaction.
func SaveRouting(staged map[string]string, resetKeys map[string]bool) error {
	return providers.UpdateRouting(func(r *providers.Routing) error {
		for _, m := range settings.Modules() {
			for _, f := range m.Fields() {
				switch {
				case resetKeys[f.Key]:
					if err := f.Set(r, ""); err != nil {
						return err
					}
				case staged[f.Key] != "":
					if err := f.Set(r, staged[f.Key]); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

// SaveCodexSettings writes the changed codex preferences to its record; saved reports whether the vault write worked.
func SaveCodexSettings(home string, old, next map[string]string) (saved bool, err error) {
	err = codex.UpdateCodexPreferences(func(current map[string]string) map[string]string {
		for _, f := range codex.CodexFields() {
			before, hadBefore := old[f.Key]
			after, hasAfter := next[f.Key]
			if hadBefore == hasAfter && before == after {
				continue
			}
			if hasAfter {
				current[f.Key] = after
			} else {
				delete(current, f.Key)
			}
		}
		return current
	})
	if err != nil {
		return false, err
	}
	return true, codex.ApplyCodexSettings(home)
}
