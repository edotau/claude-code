package tailscale

import (
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/vault"
)

// Cred is one Tailscale credential name/value pair waiting to be stored together.
type Cred struct{ Name, Value string }

// CheckCreds refuses an empty or multi-line value so a pair is never half-written.
func CheckCreds(creds []Cred) error {
	for _, c := range creds {
		if c.Value == "" {
			return fmt.Errorf("empty value for %s", c.Name)
		}
		if strings.ContainsAny(c.Value, "\r\n") { // vault.Store rejects these
			return fmt.Errorf("%s spans lines", c.Name)
		}
	}
	return nil
}

// StoreCred stores c, or with dryRun only reports; verb is "add" or "replace" for a dry run, else "".
func StoreCred(c Cred, dryRun bool) (verb string, err error) {
	if dryRun {
		if slices.Contains(vault.Names(), c.Name) {
			return "replace", nil
		}
		return "add", nil
	}
	return "", vault.Store(c.Name, c.Value)
}
