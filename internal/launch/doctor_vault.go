package launch

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
)

func vaultDoctorChecks() []Check {
	row := Check{Name: "vault", OK: true}
	count, retired := 0, 0
	_, err := os.Stat(vault.Path())
	if errors.Is(err, fs.ErrNotExist) {
		row.Detail = "not created yet"
	} else {
		if err == nil {
			err = vault.View(func(t *vault.Tx) error {
				count = len(t.Names(vault.Secrets))
				retired = len(vault.Retired(t))
				return nil
			})
		}
		source := "not needed"
		if err == nil && count > 0 {
			_, source, err = vault.MasterKey()
		}
		row.Detail = fmt.Sprintf("%s · key %s · %d secret(s) · %d retired", vault.Path(), source, count, retired)
		if err != nil {
			row.OK = false
			row.Detail += ": " + err.Error()
		}
	}
	checks, _ := secretsDoctorChecks()
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
	color := ansi.Enabled(w)
	fmt.Fprintln(w, "== vault checks ==")
	for _, c := range checks {
		mark := ansi.Style(color, ansi.Green, "✓")
		if !c.OK {
			mark = ansi.Style(color, ansi.Red, "✗")
		}
		fmt.Fprintf(w, "%s %s: %s\n", mark, c.Name, c.Detail)
	}
	return ok
}
