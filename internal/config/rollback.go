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
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
)

// Rollback preflights every source before restoring backups and archiving the vault.
func Rollback(w io.Writer) error {
	m, err := loadManifest()
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("no migration to roll back")
	}
	if err != nil {
		return err
	}
	if m.Finalizing {
		return errors.New("finalize already started; rollback refused")
	}
	var problems []error
	for _, item := range m.Items {
		if item.Done {
			_, backupErr := os.Lstat(item.Backup)
			_, sourceErr := os.Lstat(item.Source)
			// A crash between the restore rename and saving Done=false leaves exactly this state.
			if errors.Is(backupErr, fs.ErrNotExist) && sourceErr == nil {
				if err := verifyResumedRestore(item); err != nil {
					problems = append(problems, err)
				}
				continue
			}
			if backupErr != nil {
				problems = append(problems, backupErr)
			}
			if sourceErr == nil {
				problems = append(problems, fmt.Errorf("%s: source already exists", item.Source))
			} else if !errors.Is(sourceErr, fs.ErrNotExist) {
				problems = append(problems, sourceErr)
			}
		} else if _, err := os.Lstat(item.Source); err != nil {
			problems = append(problems, err)
		}
	}
	_, statErr := os.Lstat(vault.Path())
	hasVault := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		problems = append(problems, statErr)
	}
	var snapshot []byte
	if hasVault {
		snapshot, err = rollbackSnapshot(m)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", vault.Path(), err))
		}
	}
	state := filepath.Dir(manifestPath())
	var exportPath, vaultArchive, manifestArchive string
	for at := time.Now(); ; at = at.Add(time.Second) {
		ts := at.Format("20060102-150405")
		exportPath = filepath.Join(state, "vault-rollback-"+ts+".json")
		vaultArchive = vault.Path() + ".rolledback-" + ts
		manifestArchive = filepath.Join(state, "vault-migration.rolledback-"+ts+".json")
		occupied := false
		for _, path := range []string{exportPath, vaultArchive, manifestArchive} {
			if _, err := os.Lstat(path); err == nil {
				occupied = true
			} else if !errors.Is(err, fs.ErrNotExist) {
				problems = append(problems, err)
			}
		}
		if !occupied || len(problems) > 0 {
			break
		}
	}
	if len(problems) > 0 {
		for _, problem := range problems {
			fmt.Fprintln(w, problem)
		}
		return errors.New("rollback refused; nothing changed")
	}
	if hasVault {
		if err := paths.AtomicWrite(exportPath, snapshot, 0600); err != nil {
			return err
		}
	}
	for i := range m.Items {
		item := &m.Items[i]
		if !item.Done {
			continue
		}
		// Already restored by a prior crashed attempt (preflight already verified its content).
		if _, err := os.Lstat(item.Backup); errors.Is(err, fs.ErrNotExist) {
			item.Done = false
			if err := m.save(); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return err
		}
		if err := os.Rename(item.Backup, item.Source); err != nil {
			return err
		}
		item.Done = false
		if err := m.save(); err != nil {
			return err
		}
	}
	if hasVault {
		if err := os.Rename(vault.Path(), vaultArchive); err != nil {
			return err
		}
	}
	if err := os.Rename(manifestPath(), manifestArchive); err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, "rolled back migration")
	return err
}

// verifyResumedRestore checks a file already restored by a crashed rollback against the manifest's
// recorded digest; secrets items carry no digest and are trusted (the vault copy is unaffected).
func verifyResumedRestore(item Item) error {
	if item.SHA256 == "" {
		return nil
	}
	raw, err := os.ReadFile(item.Source)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != item.SHA256 {
		return fmt.Errorf("%s: restored content does not match the manifest", item.Source)
	}
	return nil
}

func rollbackSnapshot(m *Manifest) ([]byte, error) {
	doc, err := Export()
	if err != nil {
		return nil, err
	}
	imported := make(map[string]bool)
	for _, item := range m.Items {
		for _, name := range item.Names {
			imported[name] = true
		}
	}
	names := []string{}
	if err := vault.View(func(t *vault.Tx) error {
		for _, name := range t.Names(vault.Secrets) {
			if !imported[name] {
				names = append(names, name)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(struct {
		Config              json.RawMessage `json:"config"`
		SecretsNotInBackups []string        `json:"secrets_not_in_backups"`
	}{Config: doc, SecretsNotInBackups: names}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// RotateKey retains the old key so archived copies of this vault remain readable.
func RotateKey(w io.Writer) error {
	old, next, err := vault.RotateKey()
	if err != nil {
		return err
	}
	var id string
	if err := vault.View(func(t *vault.Tx) error {
		id, _ = t.Get(vault.Meta, "vault_id")
		return nil
	}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w,
		"rotated vault key %s -> %s; old key retained as vault-key/%s.%s\n",
		old, next, id, old)
	return err
}
