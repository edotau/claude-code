package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

var legacyKeyringDelete = keyring.Delete

// Finalize verifies every retained value before deleting manifest-owned migration backups.
func Finalize(w io.Writer, discard []string) error {
	m, err := loadManifest()
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("no migration to finalize")
	}
	if err != nil {
		return err
	}
	for _, item := range m.Items {
		if !item.Done {
			return errors.New("migration incomplete; re-run config migrate")
		}
	}
	// Only secret checks decrypt; a pins/env/overlay-only migration never bootstrapped a key.
	if needsKey(m) {
		if _, _, err := vault.MasterKey(); err != nil {
			return fmt.Errorf("vault locked; finalize refused: %w", err)
		}
	}
	drop := make(map[string]bool, len(discard))
	for _, name := range discard {
		drop[name] = true
	}
	for _, item := range m.Items {
		// A prior removal may have completed before its manifest save.
		if m.Finalizing {
			if _, err := os.Lstat(item.Backup); errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
		}
		if err := verifyBackup(item, drop); err != nil {
			return err
		}
	}
	var keyringNames []string
	for _, name := range m.Keyring {
		if strings.HasPrefix(name, "vault-key/") {
			continue
		}
		value, found, err := legacyKeyringLookup(name)
		if err != nil {
			return fmt.Errorf("kept %s: keyring lookup failed: %w", name, err)
		}
		if !found {
			continue
		}
		if !drop[name] {
			stored, present, err := vault.ReadSecret(name)
			if err != nil {
				return err
			}
			if !present || value != stored {
				return fmt.Errorf("kept %s: keyring value differs from the vault", name)
			}
		}
		keyringNames = append(keyringNames, name)
	}
	m.Finalizing = true
	if err := m.save(); err != nil {
		return err
	}
	for _, name := range keyringNames {
		if strings.HasPrefix(name, "vault-key/") {
			continue
		}
		if err := legacyKeyringDelete(name); err != nil {
			return fmt.Errorf("kept %s: keyring delete failed: %w", name, err)
		}
	}
	removed := 0
	for len(m.Items) > 0 {
		if err := os.Remove(m.Items[0].Backup); err == nil {
			removed++
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		m.Items = m.Items[1:]
		if err := m.save(); err != nil {
			return err
		}
	}
	if err := cleanLegacyDir(w); err != nil {
		return err
	}
	if err := os.Remove(manifestPath()); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "finalized: removed %d backup(s)\n", removed)
	return err
}

func needsKey(m *Manifest) bool {
	for _, item := range m.Items {
		if item.Kind == "secrets" {
			return true
		}
	}
	return len(m.Keyring) > 0
}

func verifyBackup(item Item, discard map[string]bool) error {
	if item.Kind != "secrets" {
		raw, err := os.ReadFile(item.Backup)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != item.SHA256 {
			return fmt.Errorf("refused: %s changed since migration", item.Backup)
		}
		return nil
	}
	values, err := parseEnvFile(item.Backup)
	if err != nil {
		return err
	}
	for _, name := range mapNames(values) {
		if discard[name] {
			continue
		}
		value, found, err := vault.ReadSecret(name)
		if err != nil {
			return err
		}
		if !found || value != values[name] {
			return fmt.Errorf(
				"refused: %s: %s differs from the vault (--discard %s to drop it)",
				item.Backup, name, name)
		}
	}
	return nil
}

func cleanLegacyDir(w io.Writer) error {
	entries, err := os.ReadDir(legacyDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".lock") {
			continue
		}
		lock := filepath.Join(legacyDir(), entry.Name())
		ran, err := paths.TryFileLock(lock, 0, func() error { return nil })
		if err != nil {
			return err
		}
		if ran {
			if err := os.Remove(lock); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	entries, err = os.ReadDir(legacyDir())
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.Remove(legacyDir())
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	_, err = fmt.Fprintf(w, "env.d kept: %s\n", strings.Join(names, ", "))
	return err
}
