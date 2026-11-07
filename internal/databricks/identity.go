// identity.go — the databricks home row and page: per-env login, PAT paste, mint, copy and agent repointing.
package databricks

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

var (
	SetRow = providers.SetProviderFields
)

// SaveIdentity writes env's identity mode and paired client id to its provider row, then seals secret (when given)
// as DATABRICKS_<ENV>_M2M, restoring the old one if the row write fails; never echoed. Mode sp needs a client id.
func SaveIdentity(reg *providers.Registry, env, mode, spID, secret string) error {
	p := reg.Providers[env]
	if p == nil {
		return fmt.Errorf("%s: no such profile", env)
	}
	if _, err := providers.ResolveIdentity(mode, ""); err != nil {
		return err
	}
	if mode == providers.IdentitySP && spID == "" {
		return fmt.Errorf("%s: identity sp needs a service principal client id", env)
	}
	name := providers.SecretName(providers.GroupDatabricks, env, providers.AuthM2M)
	var prev string
	var hadPrev bool
	if secret != "" {
		var err error
		if prev, hadPrev, err = vault.ReadSecret(name); err != nil {
			return err
		}
		if err := vault.Store(name, secret); err != nil {
			return err
		}
	}
	auth := p.Auth
	auth.SPClientID = spID
	if err := SetRow(env, map[string]any{"identity": mode, "auth": auth}); err != nil {
		if secret == "" {
			return err
		}
		return errors.Join(err, RestoreSecret(name, secret, prev, hadPrev))
	}
	return nil
}

// RestoreSecret puts record name back to prev (or deletes it) only while it still holds wrote; a newer value stays.
func RestoreSecret(name, wrote, prev string, hadPrev bool) error {
	wrote = strings.TrimSpace(wrote)
	restored := false
	err := vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		rec, ok, err := tx.Record(name)
		if err != nil || !ok {
			return err
		}
		cur, err := key.Open(name, rec)
		if err != nil {
			return err
		}
		if cur != wrote {
			return nil
		}
		if !hadPrev {
			return tx.Put(vault.Secrets, name, "")
		}
		restored = true
		return tx.PutRecord(name, vault.Record{
			Sealed: key.Seal(name, prev), Kid: key.Kid, Prefix: vault.MetaPrefix(prev), Len: len(prev), SetAt: time.Now(),
		})
	})
	if err == nil && restored {
		audit.Log(audit.SecretSet, name, "vault") // vault.Store's audit line, which this write bypasses
	}
	return err
}
