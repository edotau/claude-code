package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestFinalizeSecretValueCheckAndDiscard(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "changed"
		if missing {
			name = "deleted"
		}
		t.Run(name, func(t *testing.T) {
			kr := useMigrationKeyring(t)
			seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
			source := seedLegacyFile(t, "secrets.env", "FILE_TOKEN=original-credential\n")
			if err := Migrate(io.Discard, false); err != nil {
				t.Fatal(err)
			}
			if missing {
				if err := secrets.Delete("FILE_TOKEN"); err != nil {
					t.Fatal(err)
				}
			} else {
				vaulttest.Secret(t, "FILE_TOKEN", "new-credential")
			}
			m := readMigrationManifest(t, 2)
			manifest := readMigrationFile(t, manifestPath())
			requireMigrationError(t, Finalize(io.Discard, nil),
				"refused: "+source+".bak: FILE_TOKEN differs from the vault (--discard FILE_TOKEN to drop it)")
			requireMigrationUnchanged(t, manifest, m)
			if len(kr.deletes) != 0 {
				t.Fatal("failed value verification deleted a keyring entry")
			}
			if err := Finalize(io.Discard, []string{"FILE_TOKEN"}); err != nil {
				t.Fatal(err)
			}
			for _, item := range m.Items {
				requireMigrationFile(t, item.Backup, false)
			}
			requireMigrationFile(t, manifestPath(), false)
			requireMigrationFile(t, legacyDir(), false)
			if missing {
				requireVaultSecret(t, "FILE_TOKEN", "", false)
			} else {
				requireVaultSecret(t, "FILE_TOKEN", "new-credential", true)
			}
		})
	}
}

// verifyBackup must accept the vault value when a name's seal-time value came from the keyring, not
// the backed-up file: migrateSecrets prefers the keyring over secrets.env, so the backup's own bytes
// legitimately differ from the vault copy, and finalize must succeed without a --discard.
func TestFinalizeAcceptsKeyringValueOverFileBackup(t *testing.T) {
	kr := useMigrationKeyring(t)
	kr.values["FILE_TOKEN"] = "keyring-credential"
	seedLegacyFile(t, "secrets.env", "FILE_TOKEN=file-credential\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	requireMigrationDone(t, readMigrationManifest(t, 1))
	requireVaultSecret(t, "FILE_TOKEN", "keyring-credential", true)
	if err := Finalize(io.Discard, nil); err != nil {
		t.Fatalf("Finalize(nil) = %v, want success: the keyring value backing FILE_TOKEN still matches", err)
	}
	requireMigrationFile(t, manifestPath(), false)
}

func TestFinalizeVerifiesAllNonSecretBackupsBeforeDeleting(t *testing.T) {
	useMigrationKeyring(t)
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	seedLegacyFile(t, "custom.env", "PLAIN=value\n")
	seedLegacyFile(t, "providers.json", `{"default":"openai"}`)
	seedLegacyFile(t, "vendors.json", `{}`)
	seedLegacyFile(t, "secrets.meta.json", `{"credentials":{}}`)
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 5)
	manifest := readMigrationFile(t, manifestPath())
	for _, item := range m.Items {
		t.Run(item.Kind, func(t *testing.T) {
			original := readMigrationFile(t, item.Backup)
			writeMigrationFile(t, item.Backup, string(original)+"\n")
			requireMigrationError(t, Finalize(io.Discard, nil),
				"refused: "+item.Backup+" changed since migration")
			requireMigrationUnchanged(t, manifest, m)
			writeMigrationFile(t, item.Backup, string(original))
		})
	}
	if err := Finalize(io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	requireMigrationFile(t, manifestPath(), false)
}

// R1: a legacy providers.json overlay a prior vault.Overlay entry caused migrate to skip
// importing is still backed up and hash-verified unchanged, but its content was never put into
// the vault; finalize must notice the content is absent from the vault and refuse unless the
// caller explicitly --discard's it, instead of silently deleting the only copy of that overlay.
func TestFinalizeRefusesSkippedOverlayWithoutDiscard(t *testing.T) {
	useMigrationKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"anthropic"}`)
	source := seedLegacyFile(t, "providers.json", `{"default":"openai","legacy-only":{"env":"X"}}`)
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 1)
	if len(m.Items[0].Names) != 0 {
		t.Fatal("setup: the overlay should have been skipped as already present in the vault")
	}
	manifest := readMigrationFile(t, manifestPath())
	err := Finalize(io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "providers") {
		t.Fatalf("Finalize(nil) = %v, want a refusal naming the skipped providers overlay", err)
	}
	requireMigrationUnchanged(t, manifest, m)
	if err := Finalize(io.Discard, []string{"providers"}); err != nil {
		t.Fatalf("Finalize with providers discarded = %v, want success", err)
	}
	requireMigrationFile(t, manifestPath(), false)
	requireMigrationFile(t, source, false)
}

// R3: finalize reads and verifies a keyring value, then deletes it later by name; an old binary's
// `secrets set` can replace the keyring value in between. Finalize must re-read right before the
// delete and keep the entry (not delete it) if the value changed since verification.
func TestFinalizeKeepsKeyringEntryChangedBetweenVerifyAndDelete(t *testing.T) {
	kr := useMigrationKeyring(t)
	kr.values["META_TOKEN"] = "original-credential"
	seedLegacyFile(t, "secrets.meta.json", `{"credentials":{"META_TOKEN":{"plane":"keyring"}}}`)
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	readMigrationManifest(t, 1)
	calls := 0
	lookup := legacyKeyringLookup
	t.Cleanup(func() { legacyKeyringLookup = lookup })
	legacyKeyringLookup = func(name string) (string, bool, error) {
		if name != "META_TOKEN" {
			return lookup(name)
		}
		calls++
		if calls == 1 {
			return kr.values[name], true, nil // the verify-phase read: still matches the vault
		}
		// An old binary's `secrets set` replaced the value right before the delete would run.
		return "replaced-after-verify", true, nil
	}
	if err := Finalize(io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(kr.deletes, "META_TOKEN") {
		t.Fatal("finalize deleted a keyring entry whose value changed after verification")
	}
	requireVaultSecret(t, "META_TOKEN", "original-credential", true)
	requireMigrationFile(t, manifestPath(), false)
}

func TestFinalizeKeyringValueCheckAndDiscard(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "newer keyring value"
		if missing {
			name = "vault record missing"
		}
		t.Run(name, func(t *testing.T) {
			kr := useMigrationKeyring(t)
			kr.values["META_TOKEN"] = "original-credential"
			seedLegacyFile(t, "secrets.meta.json", `{"credentials":{"META_TOKEN":{"plane":"keyring"}}}`)
			if err := Migrate(io.Discard, false); err != nil {
				t.Fatal(err)
			}
			if missing {
				if err := secrets.Delete("META_TOKEN"); err != nil {
					t.Fatal(err)
				}
			} else {
				kr.values["META_TOKEN"] = "new-keyring-credential"
			}
			m := readMigrationManifest(t, 1)
			manifest := readMigrationFile(t, manifestPath())
			requireMigrationError(t, Finalize(io.Discard, nil),
				"kept META_TOKEN: keyring value differs from the vault")
			requireMigrationUnchanged(t, manifest, m)
			if len(kr.deletes) != 0 {
				t.Fatal("refused finalize deleted a keyring entry")
			}
			if err := Finalize(io.Discard, []string{"META_TOKEN"}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(kr.deletes, []string{"META_TOKEN"}) {
				t.Fatal("discard did not delete exactly the acknowledged keyring entry")
			}
			requireMigrationFile(t, manifestPath(), false)
		})
	}
}

func TestFinalizeKeyringFailureKeepsEverything(t *testing.T) {
	for _, lookupErr := range []error{keyring.ErrUnavailable, keyring.ErrNoAnswer} {
		t.Run(lookupErr.Error(), func(t *testing.T) {
			kr := useMigrationKeyring(t)
			kr.values["META_TOKEN"] = "original-credential"
			seedLegacyFile(t, "secrets.meta.json", `{"credentials":{"META_TOKEN":{"plane":"keyring"}}}`)
			if err := Migrate(io.Discard, false); err != nil {
				t.Fatal(err)
			}
			m := readMigrationManifest(t, 1)
			manifest := readMigrationFile(t, manifestPath())
			kr.lookupErr = lookupErr
			err := Finalize(io.Discard, []string{"META_TOKEN"})
			requireMigrationError(t, err, "kept META_TOKEN: keyring lookup failed")
			if !errors.Is(err, lookupErr) {
				t.Fatal("finalize did not preserve the keyring error")
			}
			requireMigrationUnchanged(t, manifest, m)
			if len(kr.deletes) != 0 {
				t.Fatal("unanswered keyring lookup caused a delete")
			}
		})
	}
}

func TestFinalizeKeepsUnrelatedBackupAndVaultKeyAccounts(t *testing.T) {
	kr := useMigrationKeyring(t)
	kr.values["META_TOKEN"] = "original-credential"
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	seedLegacyFile(t, "secrets.meta.json", `{"credentials":{"META_TOKEN":{"plane":"keyring"}}}`)
	unrelated := seedLegacyFile(t, "providers.json.bak-20250101-000000", "unrelated backup")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 2)
	account := "vault-key/" + m.VaultID + "." + m.Kid
	kr.values[account] = "never-delete-this-account"
	m.Keyring = append(m.Keyring, account, "CLEAN_MISS")
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	kr.lookups = nil
	var out bytes.Buffer
	if err := Finalize(&out, []string{account}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "finalized: removed 2 backup(s)") ||
		!strings.Contains(out.String(), "env.d kept: providers.json.bak-20250101-000000") {
		t.Fatal("finalize did not report owned removals and retained files")
	}
	if !reflect.DeepEqual(kr.deletes, []string{"META_TOKEN"}) ||
		slices.Contains(kr.lookups, account) || kr.values[account] == "" {
		t.Fatal("finalize touched a vault key account or a clean keyring miss")
	}
	for _, item := range m.Items {
		requireMigrationFile(t, item.Backup, false)
	}
	if string(readMigrationFile(t, unrelated)) != "unrelated backup" {
		t.Fatal("finalize changed an unrelated backup")
	}
	requireMigrationFile(t, manifestPath(), false)
	if _, _, err := vault.MasterKey(); err != nil {
		t.Fatal("vault key was not retained")
	}
	requireVaultSecret(t, "META_TOKEN", "original-credential", true)
}

func TestFinalizeLifecycleGuards(t *testing.T) {
	useMigrationKeyring(t)
	requireMigrationError(t, Finalize(io.Discard, nil), "no migration to finalize")
	seedLegacyFile(t, "secrets.env", "FILE_TOKEN=file-credential\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 1)
	m.Items[0].Done = false
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	requireMigrationError(t, Finalize(io.Discard, nil), "migration incomplete; re-run vault migrate")
	requireMigrationFile(t, m.Items[0].Backup, true)
	m.Items[0].Done = true
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	manifest := readMigrationFile(t, manifestPath())
	locked := vaulttest.Keyring(t)
	locked.Down = true
	requireMigrationError(t, Finalize(io.Discard, nil), "vault locked; finalize refused")
	requireMigrationUnchanged(t, manifest, m)
}

func TestFinalizeResumesAfterKeyringDeleteFailure(t *testing.T) {
	kr := useMigrationKeyring(t)
	kr.values["META_TOKEN"] = "original-credential"
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	seedLegacyFile(t, "secrets.meta.json", `{"credentials":{"META_TOKEN":{"plane":"keyring"}}}`)
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	kr.deleteErr = keyring.ErrUnavailable
	requireMigrationError(t, Finalize(io.Discard, nil), "kept META_TOKEN: keyring delete failed")
	m := readMigrationManifest(t, 2)
	if !m.Finalizing {
		t.Fatal("delete failure did not persist the finalizing phase")
	}
	requireMigrationDone(t, m)
	requireMigrationError(t, Rollback(io.Discard), "finalize already started; rollback refused")
	kr.deleteErr = nil
	if err := Finalize(io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	requireMigrationFile(t, manifestPath(), false)
	requireMigrationFile(t, legacyDir(), false)
}

// A backup already gone for a done item counts as already removed, with no Finalizing flag needed:
// finalize must finish and say "already gone" for each, not refuse or require a resumed-finalize state.
func TestFinalizeTreatsAlreadyGoneBackupsAsRemoved(t *testing.T) {
	useMigrationKeyring(t)
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	seedLegacyFile(t, "custom.env", "PLAIN=value\n")
	seedLegacyFile(t, "providers.json", `{"default":"openai"}`)
	seedLegacyFile(t, "vendors.json", `{}`)
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 4)
	requireMigrationDone(t, m)
	for _, item := range m.Items {
		if err := os.Remove(item.Backup); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := Finalize(&out, nil); err != nil {
		t.Fatal(err)
	}
	for _, item := range m.Items {
		if !strings.Contains(out.String(), item.Backup+": already gone\n") {
			t.Fatalf("finalize did not report %s as already gone", item.Backup)
		}
	}
	if !strings.Contains(out.String(), "finalized: removed 0 backup(s)\n") {
		t.Fatal("finalize did not report zero owned removals")
	}
	requireMigrationFile(t, manifestPath(), false)
	requireMigrationFile(t, legacyDir(), false)
}

func TestFinalizeResumesRemovedBackup(t *testing.T) {
	useMigrationKeyring(t)
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	seedLegacyFile(t, "custom.env", "PLAIN=value\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 2)
	m.Finalizing = true
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.Items[0].Backup); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Finalize(&out, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != m.Items[0].Backup+": already gone\nfinalized: removed 1 backup(s)\n" {
		t.Fatal("resumed finalize did not report the already-gone backup and remove the remaining one")
	}
	requireMigrationFile(t, manifestPath(), false)
	requireMigrationFile(t, legacyDir(), false)
}
