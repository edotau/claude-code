package tailscale

import (
	"errors"
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

// priorCred is a vault value a StoreCreds write is about to overwrite, kept so it can be restored.
type priorCred struct {
	name, value string
	had         bool
}

// StoreCreds writes every pair or none: a failed write restores the earlier pairs' previous values.
func StoreCreds(creds []Cred) error {
	var done []priorCred
	for _, c := range creds {
		old, had, err := vault.ReadSecret(c.Name)
		if err != nil {
			return errors.Join(fmt.Errorf("read %s: %w", c.Name, err), restoreCreds(done))
		}
		if err := vault.Store(c.Name, c.Value); err != nil {
			return errors.Join(err, restoreCreds(done))
		}
		done = append(done, priorCred{name: c.Name, value: old, had: had})
	}
	return nil
}

// restoreCreds puts each prior value back, newest first; a name that was absent is deleted again.
func restoreCreds(done []priorCred) error {
	var errs []error
	for _, p := range slices.Backward(done) {
		if p.had {
			errs = append(errs, vault.Store(p.name, p.value))
		} else {
			errs = append(errs, vault.Delete(p.name))
		}
	}
	return errors.Join(errs...)
}
