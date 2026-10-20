package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

type Manifest struct {
	MigratedAt time.Time `json:"migrated_at"`
	VaultID    string    `json:"vault_id"`
	Kid        string    `json:"kid,omitempty"`
	Finalizing bool      `json:"finalizing,omitempty"`
	Items      []Item    `json:"items"`
	Keyring    []string  `json:"keyring"`
}

type Item struct {
	Source string   `json:"source"`
	Backup string   `json:"backup"`
	Kind   string   `json:"kind"`
	SHA256 string   `json:"sha256,omitempty"`
	Names  []string `json:"names,omitempty"`
	Done   bool     `json:"done"`
}

func manifestPath() string {
	return filepath.Join(paths.ConfigDir(), "state", "vault-migration.json")
}

func loadManifest() (*Manifest, error) {
	raw, err := os.ReadFile(manifestPath())
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", manifestPath(), err)
	}
	return &m, nil
}

func (m *Manifest) save() error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return paths.AtomicWrite(manifestPath(), append(raw, '\n'), 0600)
}

// refuseVaultIDMismatch blocks finalize/rollback when the manifest's vault differs from the one on
// disk now (e.g. a restored/rotated vault.db since migrate ran); m.VaultID == "" predates this check.
func refuseVaultIDMismatch(m *Manifest) error {
	if m.VaultID == "" {
		return nil
	}
	var id string
	if err := vault.View(func(t *vault.Tx) error {
		id, _ = t.Get(vault.Meta, "vault_id")
		return nil
	}); err != nil {
		return err
	}
	if id != m.VaultID {
		return fmt.Errorf("refused: migration recorded vault %s, current vault is %s", m.VaultID, id)
	}
	return nil
}

// MigrationStatus reports active sources and whether a migration manifest remains.
func MigrationStatus() (pending []string, backups bool, err error) {
	pending = LegacyFiles()
	_, err = loadManifest()
	if errors.Is(err, fs.ErrNotExist) {
		return pending, false, nil
	}
	return pending, true, err
}
