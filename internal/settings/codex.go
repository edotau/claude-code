package settings

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/vault"
)

const codexOverlayKey = "codex-settings"

type CodexField struct {
	Key    string
	Values []string
}

func CodexFields() []CodexField {
	return []CodexField{
		{"model_reasoning_effort", []string{"minimal", "low", "medium", "high", "xhigh"}},
		{"model_verbosity", []string{"low", "medium", "high"}},
	}
}

func ValidateCodexOverlay(ov map[string]string) error {
	for key, value := range ov {
		found := false
		for _, f := range CodexFields() {
			if f.Key == key {
				found = true
				if !slices.Contains(f.Values, value) {
					return fmt.Errorf("codex: invalid value for %s", key)
				}
				break
			}
		}
		if !found {
			return fmt.Errorf("codex: unsupported preference %s", key)
		}
	}
	return nil
}

func loadCodexOverlay(tx *vault.Tx) (map[string]string, error) {
	ov := map[string]string{}
	if raw, ok := tx.Get(vault.Overlay, codexOverlayKey); ok {
		if err := json.Unmarshal([]byte(raw), &ov); err != nil {
			return nil, fmt.Errorf("codex: invalid saved preferences: %w", err)
		}
	}
	if ov == nil {
		ov = map[string]string{}
	}
	return ov, ValidateCodexOverlay(ov)
}

func LoadCodexOverlay() (ov map[string]string, err error) {
	err = vault.View(func(tx *vault.Tx) error { ov, err = loadCodexOverlay(tx); return err })
	return
}

func SaveCodexOverlay(ov map[string]string) error {
	return UpdateCodexOverlay(func(map[string]string) map[string]string { return ov })
}

// UpdateCodexOverlay merges staged edits in one transaction, preserving other sessions' untouched preferences.
func UpdateCodexOverlay(edit func(map[string]string) map[string]string) error {
	return vault.Update(func(tx *vault.Tx) error {
		ov, err := loadCodexOverlay(tx)
		if err != nil {
			return err
		}
		ov = edit(ov)
		if err := ValidateCodexOverlay(ov); err != nil {
			return err
		}
		raw := ""
		if len(ov) > 0 {
			data, err := json.Marshal(ov)
			if err != nil {
				return err
			}
			raw = string(data)
		}
		return tx.Put(vault.Overlay, codexOverlayKey, raw)
	})
}

func CodexConfigPath(home string) (string, error) {
	if home != "" {
		return filepath.Abs(filepath.Join(home, ".codex", "config.toml"))
	}
	dir, err := CodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

// CodexHome is $CODEX_HOME (made absolute) or ~/.codex, the directory holding codex's config.toml and auth.json.
func CodexHome() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

type CodexConfigBackup struct {
	Name    string `json:"-"`
	Path    string
	Data    []byte
	Existed bool
	At      time.Time
}

func codexPathID(path string) (string, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(abs))
	return abs, hex.EncodeToString(sum[:]), nil
}

func putCodexSealed(tx *vault.Tx, key *vault.Key, name string, data []byte) error {
	return tx.PutRecordIn(vault.SettingsBackups, name, vault.Record{
		Sealed: key.SealSettingsBackup(name, string(data)), Kid: key.Kid, SetAt: time.Now().UTC(),
	})
}

func CodexBackup(path string, raw []byte, existed bool) error {
	abs, id, err := codexPathID(path)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	prefix := "codex/" + id + "/"
	name := prefix + now.Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(nonce[:])
	data, err := json.Marshal(CodexConfigBackup{Path: abs, Data: raw, Existed: existed, At: now})
	if err != nil {
		return err
	}
	return vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		if err := putCodexSealed(tx, key, name, data); err != nil {
			return err
		}
		var names []string
		for _, n := range tx.Names(vault.SettingsBackups) {
			if strings.HasPrefix(n, prefix) {
				names = append(names, n)
			}
		}
		for _, n := range names[:max(0, len(names)-5)] {
			if err := tx.Put(vault.SettingsBackups, n, ""); err != nil {
				return err
			}
		}
		return nil
	})
}

func LatestCodexBackup(path string) (CodexConfigBackup, bool, error) {
	abs, id, err := codexPathID(path)
	if err != nil {
		return CodexConfigBackup{}, false, err
	}
	prefix, name := "codex/"+id+"/", ""
	err = vault.View(func(tx *vault.Tx) error {
		for _, n := range tx.Names(vault.SettingsBackups) {
			if strings.HasPrefix(n, prefix) {
				name = n
			}
		}
		return nil
	})
	if err != nil || name == "" {
		return CodexConfigBackup{}, false, err
	}
	raw, found, err := vault.ReadSettingsBackup(name)
	if err != nil || !found {
		return CodexConfigBackup{}, false, err
	}
	var b CodexConfigBackup
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return b, false, fmt.Errorf("codex: invalid encrypted backup: %w", err)
	}
	if b.Path != abs {
		return b, false, errors.New("codex: backup path mismatch")
	}
	b.Name = name
	return b, true, nil
}

func ConsumeCodexBackup(name string) error {
	if !strings.HasPrefix(name, "codex/") {
		return errors.New("codex: invalid backup name")
	}
	return vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.SettingsBackups, name, "") })
}

func SaveCodexBaseline(path string, raw []byte) error {
	_, id, err := codexPathID(path)
	if err != nil {
		return err
	}
	return vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error { return putCodexSealed(tx, key, "codex-baseline/"+id, raw) })
}

func LoadCodexBaseline(path string) ([]byte, error) {
	_, id, err := codexPathID(path)
	if err != nil {
		return nil, err
	}
	raw, found, err := vault.ReadSettingsBackup("codex-baseline/" + id)
	if err != nil || !found {
		return nil, err
	}
	return []byte(raw), nil
}
