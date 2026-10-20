package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

type migrationKeyring struct {
	values    map[string]string
	available bool
	lookupErr error
	deleteErr error
	lookups   []string
	deletes   []string
}

func useMigrationKeyring(t *testing.T) *migrationKeyring {
	t.Helper()
	vaulttest.UseKeyring(t)
	fake := &migrationKeyring{values: map[string]string{}, available: true}
	lookup, available, deleteEntry := legacyKeyringLookup, legacyKeyringAvailable, legacyKeyringDelete
	t.Cleanup(func() {
		legacyKeyringLookup, legacyKeyringAvailable, legacyKeyringDelete = lookup, available, deleteEntry
	})
	legacyKeyringLookup = func(name string) (string, bool, error) {
		fake.lookups = append(fake.lookups, name)
		if fake.lookupErr != nil {
			return "", false, fake.lookupErr
		}
		value, found := fake.values[name]
		return value, found, nil
	}
	legacyKeyringAvailable = func() bool { return fake.available }
	legacyKeyringDelete = func(name string) error {
		fake.deletes = append(fake.deletes, name)
		if fake.deleteErr != nil {
			return fake.deleteErr
		}
		delete(fake.values, name)
		return nil
	}
	return fake
}

func writeMigrationFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func seedLegacyFile(t *testing.T, name, content string) string {
	t.Helper()
	return writeMigrationFile(t, filepath.Join(legacyDir(), name), content)
}

func requireMigrationFile(t *testing.T, path string, present bool) {
	t.Helper()
	_, err := os.Lstat(path)
	if present && err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
	if !present && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("expected %s to be absent: %v", path, err)
	}
}

func readMigrationFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func requireMigrationError(t *testing.T, err error, text string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), text) {
		t.Fatalf("expected error containing %q", text)
	}
}

func requireVaultSecret(t *testing.T, name, want string, present bool) {
	t.Helper()
	got, found, err := vault.ReadSecret(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if found != present || got != want {
		t.Fatalf("vault secret %s has unexpected presence or value", name)
	}
}

func readMigrationManifest(t *testing.T, count int) *Manifest {
	t.Helper()
	m, err := loadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Items) != count {
		t.Fatalf("manifest has %d items, want %d", len(m.Items), count)
	}
	return m
}

func requireMigrationDone(t *testing.T, m *Manifest) {
	t.Helper()
	for _, item := range m.Items {
		if !item.Done {
			t.Errorf("%s was not marked done", item.Source)
		}
		requireMigrationFile(t, item.Source, false)
		requireMigrationFile(t, item.Backup, true)
	}
}

func requireMigrationUnchanged(t *testing.T, manifest []byte, m *Manifest) {
	t.Helper()
	if !bytes.Equal(readMigrationFile(t, manifestPath()), manifest) {
		t.Fatal("refused operation changed the manifest")
	}
	requireMigrationDone(t, m)
}

func decodeMigrationDoc(t *testing.T) Doc {
	t.Helper()
	var doc Doc
	if err := json.Unmarshal(exportTestDoc(t), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestParseEnvFileStrict(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    map[string]string
		errText string
	}{
		{
			name: "exports quotes and equals",
			content: "# comment\n\nexport ALPHA=\"one=two\"\nBETA='three four'\n" +
				"GAMMA=first\nGAMMA=last\n",
			want: map[string]string{"ALPHA": "one=two", "BETA": "three four", "GAMMA": "last"},
		},
		{
			name: "over old scanner limit", content: "LONG=" + strings.Repeat("x", 70000) + "\nAFTER=end\n",
			want: map[string]string{"LONG": strings.Repeat("x", 70000), "AFTER": "end"},
		},
		{name: "no equals", content: "BEFORE=one\nBROKEN\nAFTER=two\n", errText: "line 2: want NAME=value"},
		{
			name: "truncated", content: "BEFORE=one\nLONG=" + strings.Repeat("x", 4<<20) + "\nAFTER=two\n",
			errText: "read stopped early",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			path := seedLegacyFile(t, "secrets.env", tt.content)
			got, err := parseEnvFile(path)
			if tt.errText != "" {
				requireMigrationError(t, err, tt.errText)
				requireMigrationError(t, err, path)
				if got != nil {
					t.Fatal("invalid file returned partial values")
				}
			} else if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatal("valid dotenv file was not parsed exactly")
			}
		})
	}
}

func TestParseEnvFileOpenErrors(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := parseEnvFile(filepath.Join(legacyDir(), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("missing source did not return an open error")
	}
	if _, err := parseEnvFile(t.TempDir()); err == nil {
		t.Fatal("directory source did not return a read error")
	}
}

func TestLegacyFilesAndMigrationStatus(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	env := seedLegacyFile(t, "custom.env", "CUSTOM=plain\n")
	root := writeMigrationFile(t, rootProvidersFile(), "{}")
	seedLegacyFile(t, "custom.env.bak", "unrelated")
	seedLegacyFile(t, "provider.env.lock", "")
	want := []string{env, root}
	if got := LegacyFiles(); !reflect.DeepEqual(got, want) {
		t.Fatalf("LegacyFiles = %v, want %v", got, want)
	}
	pending, backups, err := MigrationStatus()
	if err != nil || backups || !reflect.DeepEqual(pending, want) {
		t.Fatal("status without a manifest did not report only active sources")
	}
	if err := (&Manifest{}).save(); err != nil {
		t.Fatal(err)
	}
	pending, backups, err = MigrationStatus()
	if err != nil || !backups || !reflect.DeepEqual(pending, want) {
		t.Fatal("status did not report the migration manifest")
	}
}
