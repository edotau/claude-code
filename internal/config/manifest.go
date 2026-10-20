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

// MigrationStatus reports active sources and whether a migration manifest remains.
func MigrationStatus() (pending []string, backups bool, err error) {
	pending = LegacyFiles()
	_, err = loadManifest()
	if errors.Is(err, fs.ErrNotExist) {
		return pending, false, nil
	}
	return pending, true, err
}
