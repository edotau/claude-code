package launch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
)

func vaultDoctorChecks() []Check {
	row := Check{Name: "vault", OK: true}
	retired := 0
	_, err := os.Stat(vault.Path())
	if errors.Is(err, fs.ErrNotExist) {
		row.Detail = "not created yet"
	} else {
		if err == nil {
			err = vault.View(func(t *vault.Tx) error {
				retired = len(vault.Retired(t))
				return nil
			})
		}
		count, source := 0, "not needed"
		if err == nil {
			// Shares the "vault master key" row's resolution instead of re-resolving the master key here.
			var kerr error
			count, source, kerr = secrets.MasterKeyStatus()
			err = kerr
		}
		row.Detail = fmt.Sprintf("%s · key %s · %d secret(s) · %d retired", vault.Path(), source, count, retired)
		if err != nil {
			row.OK = false
			row.Detail += ": " + err.Error()
		}
	}
	// "vault master key" repeats this row's own count/source/error; drop it here, where "vault" already
	// shows it, but keep it in secretsDoctorChecks for VaultDoctor (doctor --secrets never prints "vault").
	checks, _ := secretsDoctorChecks()
	checks = slices.DeleteFunc(checks, func(c Check) bool { return c.Name == "vault master key" })
	return append([]Check{row}, checks...)
}

// secretsDoctorChecks adapts secrets.DoctorChecks to soft doctor rows; gateOK is false when a gating row failed.
func secretsDoctorChecks() (checks []Check, gateOK bool) {
	reg, _ := providers.Load() // a load error is reported by the provider registry row; nil skips name checks
	gateOK = true
	for _, c := range secrets.DoctorChecks(reg) {
		checks = append(checks, Check{Name: c.Name, OK: c.OK, Detail: c.Detail})
		if c.Gate && !c.OK {
			gateOK = false
		}
	}
	return checks, gateOK
}

// VaultDoctor prints the vault rows as ✓/✗ lines for doctor --secrets; false when a gating row failed.
func VaultDoctor(w io.Writer) bool {
	checks, ok := secretsDoctorChecks()
	fmt.Fprintln(w, "== vault checks ==")
	printChecks(w, checks)
	return ok
}
