package launch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/edotau/claude-code/internal/config"
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
	checks := []Check{row}
	pending, backups, err := config.MigrationStatus()
	if len(pending) > 0 {
		checks = append(checks, Check{Name: "legacy env.d", Detail: fmt.Sprintf(
			"%d file(s); run claude-code vault migrate", len(pending))})
	}
	if backups || err != nil {
		detail := "run claude-code vault finalize when verified"
		if err != nil {
			detail = err.Error()
		}
		checks = append(checks, Check{Name: "migration backups", Detail: detail})
	}
	return checks
}
