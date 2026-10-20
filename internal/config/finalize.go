package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
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
			return errors.New("migration incomplete; re-run vault migrate")
		}
	}
	if err := refuseVaultIDMismatch(m); err != nil {
		return err
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
		// A prior removal, or a backup deleted by hand, leaves nothing to verify; count it as gone.
		if _, err := os.Lstat(item.Backup); errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(w, "%s: already gone\n", item.Backup)
			continue
		} else if err != nil {
			return err
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
		// An old binary's `secrets set` can replace the keyring value between the verify loop
		// above and this delete; re-read and re-compare right before deleting to catch that
		// window (a race still remains between this read and the delete call itself).
		if !drop[name] {
			current, found, err := legacyKeyringLookup(name)
			if err != nil {
				return fmt.Errorf("kept %s: keyring lookup failed: %w", name, err)
			}
			if !found {
				continue
			}
			stored, present, err := vault.ReadSecret(name)
			if err != nil {
				return err
			}
			if !present || current != stored {
				fmt.Fprintf(w, "kept %s: keyring value changed since verification\n", name)
				continue
			}
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
		if discard[item.Kind] {
			return nil
		}
		switch item.Kind {
		case "pins", "env":
			return verifyEnvImported(item)
		case "providers", "vendors":
			return verifyOverlayImported(item, raw)
		default:
			return nil // secrets-meta: metadata only, nothing stored under its own name to check
		}
	}
	values, err := parseEnvFile(item.Backup)
	if err != nil {
		return err
	}
	for _, name := range mapNames(values) {
		if discard[name] {
			continue
		}
		expect, err := expectedImportedValue(name, values[name])
		if err != nil {
			return err
		}
		value, found, err := vault.ReadSecret(name)
		if err != nil {
			return err
		}
		if !found || value != expect {
			return fmt.Errorf(
				"refused: %s: %s differs from the vault (--discard %s to drop it)",
				item.Backup, name, name)
		}
	}
	return nil
}

// verifyEnvImported confirms a migrate skip (the vault already held a pin/env name) did not leave
// that name's value only in the backup; the @keyring -> @secret rewrite is accounted for.
func verifyEnvImported(item Item) error {
	values, err := parseEnvFile(item.Backup)
	if err != nil {
		return err
	}
	bucket := vault.Pins
	if item.Kind == "env" {
		bucket = vault.Env
	}
	var missing []string
	if err := vault.View(func(t *vault.Tx) error {
		for _, name := range mapNames(values) {
			want := values[name]
			if item.Kind == "env" && want == "@keyring" {
				want = providers.SecretRef
			}
			got, ok := t.Get(bucket, name)
			if !ok || got != want {
				missing = append(missing, name)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if len(missing) != 0 {
		return fmt.Errorf("refused: %s: %s not present in the vault (--discard %s to drop it)",
			item.Backup, strings.Join(missing, ", "), item.Kind)
	}
	return nil
}

// verifyOverlayImported confirms a migrate skip (the vault already held this overlay) did not
// strand legacy-only provider/vendor entries that were never merged into the stored document.
func verifyOverlayImported(item Item, raw []byte) error {
	var stored string
	found := false
	if err := vault.View(func(t *vault.Tx) error {
		stored, found = t.Get(vault.Overlay, item.Kind)
		return nil
	}); err != nil {
		return err
	}
	if found && stored == string(raw) {
		return nil
	}
	var legacyTop map[string]json.RawMessage
	if err := json.Unmarshal(raw, &legacyTop); err != nil {
		return err
	}
	var storedTop map[string]json.RawMessage
	if found {
		if err := json.Unmarshal([]byte(stored), &storedTop); err != nil {
			return err
		}
	}
	var missing []string
	for key, val := range legacyTop {
		if strings.TrimSpace(string(val)) == "null" {
			continue // a tombstone in the legacy doc deletes; nothing to preserve
		}
		sv, ok := storedTop[key]
		if !ok || !jsonRawEqual(val, sv) {
			missing = append(missing, key)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return fmt.Errorf("refused: %s: %s %s not present in the vault (--discard %s to drop it)",
			item.Backup, item.Kind, strings.Join(missing, ", "), item.Kind)
	}
	return nil
}

func jsonRawEqual(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
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
