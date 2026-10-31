package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
)

// secretName is the router secret's vault record.
const secretName = "CLAUDE_CODE_ROUTER_SECRET"

// ClientSecret returns the per-install secret clients must present, sealed in the vault.
// First use imports state/router.secret (same value), reads it back, then deletes the file.
// Migration: remove after 2026-11-13 once every machine has run a post-2026-10-28 build.
func ClientSecret() (string, error) {
	s, found, err := vault.ReadSecret(secretName)
	if err != nil {
		return "", secretErr(err)
	}
	if found && s != "" {
		return s, nil
	}
	var out string
	err = paths.WithFileLock(secretFile(), func() error {
		var err error
		out, err = initSecret()
		return err
	})
	return out, err
}

// initSecret runs under the file lock: re-check the vault, import or mint, verify, then drop the file.
func initSecret() (string, error) {
	if s, found, err := vault.ReadSecret(secretName); err != nil {
		return "", secretErr(err)
	} else if found && s != "" {
		return s, nil
	}
	val, fromFile := "", false
	if b, err := os.ReadFile(secretFile()); err == nil {
		val, fromFile = strings.TrimSpace(string(b)), true
	}
	if val == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		val = hex.EncodeToString(b)
	}
	if err := secrets.Store(secretName, val); err != nil {
		return "", secretErr(err)
	}
	if got, found, err := vault.ReadSecret(secretName); err != nil || !found || got != val {
		return "", fmt.Errorf("proxy: router secret did not read back from the vault (kept %s)", secretFile())
	}
	if fromFile {
		if err := os.Remove(secretFile()); err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	return val, nil
}

func secretErr(err error) error {
	return fmt.Errorf("proxy: router secret needs the vault (see `claude-code vault`): %w", err)
}

// authHint names the remedy a rejected credential needs, picked by the provider's auth type.
func authHint(p *providers.Provider) string {
	a := p.Auth
	switch {
	case p.Preset == providers.PresetDatabricks || a.Type == providers.AuthOAuth || a.Type == providers.AuthM2M:
		return "run `claude-code databricks login --profile " + p.Name + "`"
	case a.Command != "":
		return "check the provider's auth.command"
	case (a.Type == providers.AuthKey || a.Type == providers.AuthPAT) && a.Env != "":
		return "run `claude-code vault set " + a.Env + "`"
	}
	return "run `claude-code configure provider --list " + p.Name + "`"
}
