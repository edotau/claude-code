package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// secretName is the router secret's vault record.
const secretName = providers.RouterSecretName

// ClientSecret reads the vault secret, importing router.secret if needed; remove migration after 2026-11-13.
func ClientSecret() (string, error) {
	if s, ok, err := vaultSecret(); err != nil || ok {
		return s, err
	}
	var out string
	err := paths.WithFileLock(secretFile(), func() error {
		var err error
		out, err = initSecret()
		return err
	})
	return out, err
}

// initSecret runs under the file lock: re-check the vault, import or mint, verify, then drop the file.
func initSecret() (string, error) {
	if s, ok, err := vaultSecret(); err != nil || ok {
		return s, err
	}
	val, fromFile, err := legacyOrNewSecret()
	if err != nil {
		return "", err
	}
	if err := storeVerified(val); err != nil {
		return "", err
	}
	if fromFile {
		if err := os.Remove(secretFile()); err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	return val, nil
}

// vaultSecret reads the vault record; ok is false when it is absent or empty.
func vaultSecret() (s string, ok bool, err error) {
	s, found, err := vault.ReadSecret(secretName)
	if err != nil {
		return "", false, secretErr(err)
	}
	return s, found && s != "", nil
}

// legacyOrNewSecret returns the legacy router.secret file's value, or a fresh random one when it is absent or empty.
func legacyOrNewSecret() (val string, fromFile bool, err error) {
	if b, err := os.ReadFile(secretFile()); err == nil {
		val, fromFile = strings.TrimSpace(string(b)), true
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("proxy: read %s: %w", secretFile(), err)
	}
	if val == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", false, err
		}
		val = hex.EncodeToString(b)
	}
	return val, fromFile, nil
}

// storeVerified writes val to the vault and confirms it reads back unchanged.
func storeVerified(val string) error {
	if err := vault.Store(secretName, val); err != nil {
		return secretErr(err)
	}
	if got, found, err := vault.ReadSecret(secretName); err != nil || !found || got != val {
		return fmt.Errorf("proxy: router secret did not read back from the vault (kept %s)", secretFile())
	}
	return nil
}

func secretErr(err error) error {
	return fmt.Errorf("proxy: router secret needs the vault (see `claude-code vault`): %w", err)
}
