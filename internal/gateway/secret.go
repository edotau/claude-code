package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// secretName is the router secret's vault record.
const secretName = providers.RouterSecretName

// ClientSecret reads the vault secret, else mints and stores one under the lock.
func ClientSecret() (string, error) {
	if s, ok, err := vaultSecret(); err != nil || ok {
		return s, err
	}
	var out string
	err := paths.WithFileLock(secretLockFile(), func() error {
		var err error
		out, err = initSecret()
		return err
	})
	return out, err
}

// initSecret runs under the file lock: re-check the vault, else mint, store and verify.
func initSecret() (string, error) {
	if s, ok, err := vaultSecret(); err != nil || ok {
		return s, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	val := hex.EncodeToString(b)
	if err := storeVerified(val); err != nil {
		return "", err
	}
	return val, nil
}

// vaultSecret reads the vault record; ok is false when absent or empty.
func vaultSecret() (s string, ok bool, err error) {
	s, found, err := vault.ReadSecret(secretName)
	if err != nil {
		return "", false, secretErr(err)
	}
	return s, found && s != "", nil
}

// storeVerified writes val to the vault and confirms it reads back unchanged.
func storeVerified(val string) error {
	if err := vault.Store(secretName, val); err != nil {
		return secretErr(err)
	}
	if got, found, err := vault.ReadSecret(secretName); err != nil || !found || got != val {
		return errors.New("proxy: router secret did not read back from the vault")
	}
	return nil
}

func secretErr(err error) error {
	return fmt.Errorf("proxy: router secret needs the vault (see `claude-code vault`): %w", err)
}
