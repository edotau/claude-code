package settings_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestConfigHistoryIsEncryptedBoundedAndSurvivesRotation(t *testing.T) {
	dir, _ := vaulttest.UseKeyring(t)
	path := filepath.Join(dir, "native", "config.toml")
	other := filepath.Join(dir, "other", "config.toml")
	fixture := []byte("private-config-fixture-68b19d\nforeign = 'preserve exactly'\n")
	if err := settings.SaveCodexBaseline(path, fixture); err != nil {
		t.Fatal(err)
	}
	for i := range 7 {
		if err := settings.CodexBackup(path, []byte(fmt.Sprintf("version %d", i)), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := settings.CodexBackup(other, fixture, false); err != nil {
		t.Fatal(err)
	}
	vaulttest.Secret(t, "API_KEY", "credential-fixture")
	pairs, err := secrets.ExportPairs(nil)
	if err != nil || !reflect.DeepEqual(pairs, []secrets.Pair{{Name: "API_KEY", Value: "credential-fixture"}}) {
		t.Fatalf("credential export includes unexpected entries: %v, %v", pairs, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state", "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, clear := range [][]byte{fixture, []byte(base64.StdEncoding.EncodeToString(fixture)), []byte("private-config-fixture-68b19d")} {
		if bytes.Contains(raw, clear) {
			t.Fatal("vault contains plaintext config contents")
		}
	}
	oldKid, newKid, err := vault.RotateKey()
	if err != nil || oldKid == newKid {
		t.Fatalf("rotation did not change the key: %v", err)
	}
	baseline, err := settings.LoadCodexBaseline(path)
	if err != nil || !bytes.Equal(baseline, fixture) {
		t.Fatalf("baseline did not survive rotation: %v", err)
	}
	pairs, err = secrets.ExportPairs(nil)
	if err != nil || !reflect.DeepEqual(pairs, []secrets.Pair{{Name: "API_KEY", Value: "credential-fixture"}}) {
		t.Fatalf("credential did not survive rotation: %v", err)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		for _, name := range tx.Names(vault.SettingsBackups) {
			r, found, err := tx.RecordIn(vault.SettingsBackups, name)
			if err != nil || !found || r.Kid != newKid || r.Prefix != "" {
				t.Errorf("backup metadata after rotation: found=%v, kid=%q, prefix=%q, err=%v", found, r.Kid, r.Prefix, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 6; i >= 2; i-- {
		b, found, err := settings.LatestCodexBackup(path)
		if err != nil || !found || string(b.Data) != fmt.Sprintf("version %d", i) || !b.Existed || b.Path != path {
			t.Fatalf("latest backup %d: found=%v, data=%q, err=%v", i, found, b.Data, err)
		}
		if err := settings.ConsumeCodexBackup(b.Name); err != nil {
			t.Fatal(err)
		}
	}
	if _, found, err := settings.LatestCodexBackup(path); err != nil || found {
		t.Fatalf("more than five snapshots retained: found=%v, err=%v", found, err)
	}
	b, found, err := settings.LatestCodexBackup(other)
	if err != nil || !found || !bytes.Equal(b.Data, fixture) || b.Existed || b.Path != other {
		t.Fatalf("other path snapshot changed: found=%v, err=%v", found, err)
	}
	baseline, err = settings.LoadCodexBaseline(path)
	if err != nil || !bytes.Equal(baseline, fixture) {
		t.Fatalf("snapshot consumption changed baseline: %v", err)
	}
}

func TestCredentialAndConfigCiphertextCannotBeExchanged(t *testing.T) {
	vaulttest.UseKeyring(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := settings.CodexBackup(path, []byte("original native config"), true); err != nil {
		t.Fatal(err)
	}
	b, found, err := settings.LatestCodexBackup(path)
	if err != nil || !found {
		t.Fatalf("backup missing: %v", err)
	}
	vaulttest.Secret(t, b.Name, "credential using identical record name")
	if err := vault.Update(func(tx *vault.Tx) error {
		credential, _, err := tx.Record(b.Name)
		if err != nil {
			return err
		}
		backup, _, err := tx.RecordIn(vault.SettingsBackups, b.Name)
		if err != nil {
			return err
		}
		if err := tx.PutRecord(b.Name, backup); err != nil {
			return err
		}
		return tx.PutRecordIn(vault.SettingsBackups, b.Name, credential)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := vault.ReadSecret(b.Name); err == nil {
		t.Fatal("credential collection decrypted settings ciphertext")
	}
	if _, _, err := vault.ReadSettingsBackup(b.Name); err == nil {
		t.Fatal("settings collection decrypted credential ciphertext")
	}
}

func TestPreferencesRejectInvalidEditsWithoutOverwritingSavedValues(t *testing.T) {
	vaulttest.Use(t)
	want := map[string]string{"model_reasoning_effort": "high", "model_verbosity": "low"}
	if err := settings.SaveCodexOverlay(want); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []map[string]string{
		{"model_reasoning_effort": "unbounded"},
		{"model_verbosity": "verbose"},
		{"max_output_tokens": "256"},
	} {
		if err := settings.SaveCodexOverlay(invalid); err == nil {
			t.Fatalf("accepted invalid preferences: %v", invalid)
		}
		got, err := settings.LoadCodexOverlay()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("invalid edit overwrote saved preferences: %v, %v", got, err)
		}
	}
	if err := settings.UpdateCodexOverlay(func(current map[string]string) map[string]string {
		current["model_verbosity"] = "medium"
		return current
	}); err != nil {
		t.Fatal(err)
	}
	want["model_verbosity"] = "medium"
	got, err := settings.LoadCodexOverlay()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("staged preference merge lost an untouched preference: %v, %v", got, err)
	}
}

func TestConfigPathPrecedence(t *testing.T) {
	home, native := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", native)
	for _, tc := range []struct{ home, want string }{
		{home, filepath.Join(home, ".codex", "config.toml")},
		{"", filepath.Join(native, "config.toml")},
	} {
		got, err := settings.CodexConfigPath(tc.home)
		if err != nil || got != tc.want {
			t.Fatalf("ConfigPath(%q) = %q, %v; want %q", tc.home, got, err, tc.want)
		}
	}
	t.Setenv("CODEX_HOME", "")
	defaultHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := settings.CodexConfigPath(""); err != nil || got != filepath.Join(defaultHome, ".codex", "config.toml") {
		t.Fatalf("default ConfigPath = %q, %v", got, err)
	}
}
