package signin

import (
	"fmt"

	"github.com/edotau/claude-code/internal/secrets"
)

// ValidateImport checks every pair so a bad one aborts before anything is written.
func ValidateImport(pairs []secrets.Pair) error {
	for _, p := range pairs {
		if err := ValidateSecret(p.Name, p.Value); err != nil {
			return err
		}
	}
	return nil
}

// StoreImport stores each pair in order, calling stored after each success; an error names the failing pair.
func StoreImport(pairs []secrets.Pair, stored func(name string)) error {
	for _, p := range pairs {
		if err := SetSecret(p.Name, p.Value); err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		stored(p.Name)
	}
	return nil
}
