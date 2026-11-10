package configure

import (
	"fmt"
	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/vault"
)

// ValidateImport checks every pair so a bad one aborts before anything is written.
func ValidateImport(pairs []vault.Pair) error {
	for _, p := range pairs {
		if err := agent.ValidateVaultSecret(p.Name, p.Value); err != nil {
			return err
		}
	}
	return nil
}

// StoreImport stores each pair in order, calling stored after each success; an error names the failing pair.
func StoreImport(pairs []vault.Pair, stored func(name string)) error {
	for _, p := range pairs {
		if err := agent.SetSecret(p.Name, p.Value); err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		stored(p.Name)
	}
	return nil
}
