package config

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func migrationMatches(t *testing.T, pattern string, count int) []string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != count {
		t.Fatalf("%s matched %d files, want %d", pattern, len(matches), count)
	}
	return matches
}

func TestRollbackPreflightReportsAllProblemsWithoutMutation(t *testing.T) {
	kr := useMigrationKeyring(t)
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	seedLegacyFile(t, "custom.env", "PLAIN=value\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 2)
	writeMigrationFile(t, m.Items[0].Source, "occupied destination")
	if err := os.Remove(m.Items[1].Backup); err != nil {
		t.Fatal(err)
	}
	manifest := readMigrationFile(t, manifestPath())
	db := readMigrationFile(t, vault.Path())
	backup := readMigrationFile(t, m.Items[0].Backup)
	var out bytes.Buffer
	requireMigrationError(t, Rollback(&out), "rollback refused; nothing changed")
	if !strings.Contains(out.String(), m.Items[0].Source+": source already exists") ||
		!strings.Contains(out.String(), m.Items[1].Backup) {
		t.Fatal("preflight did not report both the occupied source and missing backup")
	}
	if !bytes.Equal(readMigrationFile(t, manifestPath()), manifest) ||
		!bytes.Equal(readMigrationFile(t, vault.Path()), db) ||
		!bytes.Equal(readMigrationFile(t, m.Items[0].Backup), backup) ||
		string(readMigrationFile(t, m.Items[0].Source)) != "occupied destination" {
		t.Fatal("failed preflight mutated a manifest, vault, source, or backup")
	}
	requireMigrationFile(t, m.Items[1].Source, false)
	state := filepath.Dir(manifestPath())
	migrationMatches(t, filepath.Join(state, "vault-rollback-*.json"), 0)
	migrationMatches(t, filepath.Join(state, "*.rolledback-*"), 0)
	if len(kr.deletes) != 0 {
		t.Fatal("rollback touched the keyring")
	}
}

// Rollback must refuse to silently revert a secret `secrets set` changed since migrate, listing
// only the changed name (never a value, old or new) in its refusal, and succeed once discarded.
func TestRollbackRefusesChangedSecretWithoutDiscard(t *testing.T) {
	useMigrationKeyring(t)
	seedLegacyFile(t, "secrets.env", "FILE_TOKEN=original-credential\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 1)
	vaulttest.Secret(t, "FILE_TOKEN", "changed-after-migrate")
	manifest := readMigrationFile(t, manifestPath())
	var out bytes.Buffer
	requireMigrationError(t, Rollback(&out), "rollback refused; nothing changed")
	if !strings.Contains(out.String(), "refused: FILE_TOKEN changed in the vault since migration") {
		t.Fatalf("preflight output did not name the changed secret: %q", out.String())
	}
	for _, value := range []string{"original-credential", "changed-after-migrate"} {
		if strings.Contains(out.String(), value) {
			t.Fatal("rollback refusal printed a secret value")
		}
	}
	requireMigrationUnchanged(t, manifest, m)
	var out2 bytes.Buffer
	if err := Rollback(&out2, "FILE_TOKEN"); err != nil {
		t.Fatalf("Rollback with FILE_TOKEN discarded = %v, want success", err)
	}
	requireMigrationFile(t, manifestPath(), false)
}

// Finalize and Rollback must both refuse a manifest recorded against a vault that is no longer the
// one on disk (e.g. a restored/rotated vault.db) - acting on it would verify/restore against the
// wrong secrets.
func TestFinalizeAndRollbackRefuseVaultIDMismatch(t *testing.T) {
	useMigrationKeyring(t)
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 1)
	if m.VaultID == "" {
		t.Fatal("setup: migration did not record a vault id")
	}
	vaulttest.Put(t, vault.Meta, "vault_id", "a-different-vault-id")
	requireMigrationError(t, Finalize(io.Discard, nil),
		"refused: migration recorded vault "+m.VaultID+", current vault is a-different-vault-id")
	var out bytes.Buffer
	requireMigrationError(t, Rollback(&out), "rollback refused; nothing changed")
	if !strings.Contains(out.String(),
		"refused: migration recorded vault "+m.VaultID+", current vault is a-different-vault-id") {
		t.Fatalf("rollback preflight did not name the vault id mismatch: %q", out.String())
	}
	requireMigrationFile(t, manifestPath(), true)
	requireMigrationDone(t, m)
}

func TestRollbackPreflightRejectsUnreadableVault(t *testing.T) {
	useMigrationKeyring(t)
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 1)
	manifest := readMigrationFile(t, manifestPath())
	writeMigrationFile(t, vault.Path(), "not a bolt database")
	var out bytes.Buffer
	requireMigrationError(t, Rollback(&out), "rollback refused; nothing changed")
	if !strings.Contains(out.String(), vault.Path()) {
		t.Fatal("preflight did not identify the unreadable vault")
	}
	requireMigrationUnchanged(t, manifest, m)
	if string(readMigrationFile(t, vault.Path())) != "not a bolt database" {
		t.Fatal("preflight changed the unreadable vault")
	}
}

func TestRollbackRestoresArchivesAndReimports(t *testing.T) {
	kr := useMigrationKeyring(t)
	kr.values["FILE_TOKEN"] = "original-credential"
	pins := seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	source := seedLegacyFile(t, "secrets.env", "FILE_TOKEN=original-credential\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	vaulttest.Secret(t, "VAULT_ONLY", "vault-only-credential")
	vaulttest.Put(t, vault.Env, "ADDED_ENV", "added")
	wantDoc := exportTestDoc(t)
	m := readMigrationManifest(t, 2)
	db := readMigrationFile(t, vault.Path())
	lookups := len(kr.lookups)
	var out bytes.Buffer
	if err := Rollback(&out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "rolled back migration\n" {
		t.Fatal("rollback did not report completion")
	}
	for _, item := range m.Items {
		requireMigrationFile(t, item.Source, true)
		requireMigrationFile(t, item.Backup, false)
	}
	if string(readMigrationFile(t, pins)) != "HARNESS_PROVIDER=openai\n" ||
		string(readMigrationFile(t, source)) != "FILE_TOKEN=original-credential\n" {
		t.Fatal("rollback did not restore original source bytes")
	}
	requireMigrationFile(t, vault.Path(), false)
	requireMigrationFile(t, manifestPath(), false)
	state := filepath.Dir(manifestPath())
	archive := migrationMatches(t, vault.Path()+".rolledback-*", 1)[0]
	if !bytes.Equal(readMigrationFile(t, archive), db) {
		t.Fatal("rollback changed the archived vault bytes")
	}
	manifestArchive := migrationMatches(t, filepath.Join(state, "vault-migration.rolledback-*.json"), 1)[0]
	var restored Manifest
	if err := json.Unmarshal(readMigrationFile(t, manifestArchive), &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.Items) != 2 {
		t.Fatal("archived manifest lost items")
	}
	for _, item := range restored.Items {
		if item.Done {
			t.Error("restored source is still marked done in the archived manifest")
		}
	}
	snapshot := migrationMatches(t, filepath.Join(state, "vault-rollback-*.json"), 1)[0]
	raw := readMigrationFile(t, snapshot)
	var exported struct {
		Config              json.RawMessage `json:"config"`
		SecretsNotInBackups []string        `json:"secrets_not_in_backups"`
	}
	if err := json.Unmarshal(raw, &exported); err != nil {
		t.Fatal(err)
	}
	var want, got Doc
	if json.Unmarshal(wantDoc, &want) != nil || json.Unmarshal(exported.Config, &got) != nil ||
		!reflect.DeepEqual(want, got) ||
		!reflect.DeepEqual(exported.SecretsNotInBackups, []string{"VAULT_ONLY"}) {
		t.Fatal("rollback snapshot did not preserve config and only vault-only secret names")
	}
	for _, secret := range []string{"original-credential", "vault-only-credential"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("rollback snapshot contains a secret value")
		}
	}
	info, err := os.Stat(snapshot)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("rollback snapshot permissions are not 0600")
	}
	if len(kr.lookups) != lookups || len(kr.deletes) != 0 || kr.values["FILE_TOKEN"] == "" {
		t.Fatal("rollback touched the legacy keyring")
	}
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	requireMigrationDone(t, readMigrationManifest(t, 2))
	requireVaultSecret(t, "FILE_TOKEN", "original-credential", true)
	requireVaultSecret(t, "VAULT_ONLY", "", false)
	requireMigrationFile(t, archive, true)
}

func TestRollbackResumesRestoredItems(t *testing.T) {
	for _, vaultMoved := range []bool{false, true} {
		name := "partially restored"
		if vaultMoved {
			name = "vault already archived"
		}
		t.Run(name, func(t *testing.T) {
			useMigrationKeyring(t)
			seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
			seedLegacyFile(t, "custom.env", "PLAIN=value\n")
			if err := Migrate(io.Discard, false); err != nil {
				t.Fatal(err)
			}
			m := readMigrationManifest(t, 2)
			restored := 1
			if vaultMoved {
				restored = len(m.Items)
			}
			for i := 0; i < restored; i++ {
				if err := os.Rename(m.Items[i].Backup, m.Items[i].Source); err != nil {
					t.Fatal(err)
				}
				m.Items[i].Done = false
			}
			if err := m.save(); err != nil {
				t.Fatal(err)
			}
			if vaultMoved {
				if err := os.Rename(vault.Path(), vault.Path()+".rolledback-20260101-000000"); err != nil {
					t.Fatal(err)
				}
			}
			if err := Rollback(io.Discard); err != nil {
				t.Fatal(err)
			}
			for _, item := range m.Items {
				requireMigrationFile(t, item.Source, true)
				requireMigrationFile(t, item.Backup, false)
			}
			requireMigrationFile(t, vault.Path(), false)
			requireMigrationFile(t, manifestPath(), false)
			migrationMatches(t, vault.Path()+".rolledback-*", 1)
			archive := migrationMatches(t,
				filepath.Join(filepath.Dir(manifestPath()), "vault-migration.rolledback-*.json"), 1)[0]
			var finished Manifest
			if err := json.Unmarshal(readMigrationFile(t, archive), &finished); err != nil {
				t.Fatal(err)
			}
			for _, item := range finished.Items {
				if item.Done {
					t.Fatal("resumed rollback did not persist the restored Done flags")
				}
			}
		})
	}
}

// R6: a crash right after Rollback's os.Rename succeeds but before m.save() persists Done=false
// leaves the on-disk manifest saying Done=true while the filesystem already shows the restore
// complete (source present, backup absent). The next rollback must treat that item as already
// restored instead of rejecting both filesystem conditions and getting permanently stuck.
func TestRollbackResumesCrashBetweenRenameAndSave(t *testing.T) {
	useMigrationKeyring(t)
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	seedLegacyFile(t, "custom.env", "PLAIN=value\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 2)
	// Manually perform the rename Rollback would do for item 0, but leave Done=true on disk,
	// simulating a crash between the os.Rename and the manifest save that follows it.
	if err := os.Rename(m.Items[0].Backup, m.Items[0].Source); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, item := range m.Items {
		requireMigrationFile(t, item.Source, true)
		requireMigrationFile(t, item.Backup, false)
	}
	requireMigrationFile(t, vault.Path(), false)
	requireMigrationFile(t, manifestPath(), false)
}

func TestRollbackLifecycleGuards(t *testing.T) {
	useMigrationKeyring(t)
	requireMigrationError(t, Rollback(io.Discard), "no migration to roll back")
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 1)
	m.Finalizing = true
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	requireMigrationError(t, Rollback(io.Discard), "finalize already started; rollback refused")
	m.Finalizing = false
	m.Items[0].Done = false
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	manifest := readMigrationFile(t, manifestPath())
	var out bytes.Buffer
	requireMigrationError(t, Rollback(&out), "rollback refused; nothing changed")
	if !strings.Contains(out.String(), m.Items[0].Source) {
		t.Fatal("preflight did not report a missing previously restored source")
	}
	if !bytes.Equal(readMigrationFile(t, manifestPath()), manifest) {
		t.Fatal("failed resume changed the manifest")
	}
	requireMigrationFile(t, m.Items[0].Backup, true)
	requireMigrationFile(t, vault.Path(), true)
}
