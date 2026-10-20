package config

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestMigrateAllGroupsAndIdempotency(t *testing.T) {
	kr := useMigrationKeyring(t)
	kr.values["META_TOKEN"] = "metadata-credential"
	kr.values["REF_TOKEN"] = "reference-credential"
	kr.values["FILE_TOKEN"] = "keyring-preferred"
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	seedLegacyFile(t, "custom.env", "REF_TOKEN=@keyring\nPLAIN=value\n")
	seedLegacyFile(t, "providers.json", `{"default":"openai"}`)
	seedLegacyFile(t, "vendors.json", `{"vendors":{"copilot":null}}`)
	seedLegacyFile(t, "secrets.env", "FILE_TOKEN=file-credential\nFILE_ONLY=file-only-credential\n")
	seedLegacyFile(t, "secrets.meta.json", `{"version":1,"credentials":{"META_TOKEN":{
		"plane":"keyring","set_at":"2026-01-02T03:04:05Z",
		"verified_at":"2026-02-03T04:05:06Z","verify_status":"ok"}}}`)
	oldBackup := seedLegacyFile(t, "provider.env.bak", "unrelated backup")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 6)
	requireMigrationDone(t, m)
	if m.VaultID == "" || m.Kid == "" || m.MigratedAt.IsZero() || m.Finalizing {
		t.Fatal("manifest identity or lifecycle fields were not populated")
	}
	if !reflect.DeepEqual(m.Keyring, []string{"FILE_TOKEN", "META_TOKEN", "REF_TOKEN"}) {
		t.Fatalf("unexpected imported keyring names: %v", m.Keyring)
	}
	for _, item := range m.Items {
		if (item.Kind == "secrets") != (item.SHA256 == "") {
			t.Errorf("unexpected digest presence for %s", item.Kind)
		}
		if item.Source+".bak" == oldBackup && !strings.HasPrefix(item.Backup, oldBackup+"-") {
			t.Fatal("existing backup was not given a collision-safe successor")
		}
	}
	if string(readMigrationFile(t, oldBackup)) != "unrelated backup" {
		t.Fatal("unrelated backup changed")
	}
	info, err := os.Stat(manifestPath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("manifest permissions are not 0600")
	}
	doc := decodeMigrationDoc(t)
	if doc.Pins["HARNESS_PROVIDER"] != "openai" ||
		doc.Env["REF_TOKEN"] != "@secret" || doc.Env["PLAIN"] != "value" {
		t.Fatal("pins, plain env, or @keyring rewrite did not survive Export")
	}
	var providers, vendors map[string]json.RawMessage
	if json.Unmarshal(doc.Providers, &providers) != nil || string(providers["default"]) != `"openai"` ||
		json.Unmarshal(doc.Vendors, &vendors) != nil || len(vendors["vendors"]) == 0 {
		t.Fatal("provider or vendor overlay was not imported")
	}
	requireVaultSecret(t, "FILE_TOKEN", kr.values["FILE_TOKEN"], true)
	requireVaultSecret(t, "FILE_ONLY", "file-only-credential", true)
	requireVaultSecret(t, "META_TOKEN", kr.values["META_TOKEN"], true)
	requireVaultSecret(t, "REF_TOKEN", kr.values["REF_TOKEN"], true)
	if err := vault.View(func(tx *vault.Tx) error {
		r, found, err := tx.Record("META_TOKEN")
		if err != nil {
			return err
		}
		if !found || r.SetAt.Format(time.RFC3339) != "2026-01-02T03:04:05Z" ||
			r.VerifiedAt.Format(time.RFC3339) != "2026-02-03T04:05:06Z" || r.VerifyStatus != "ok" {
			t.Error("legacy credential metadata was not preserved")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := vaultTestState(t)
	manifest := readMigrationFile(t, manifestPath())
	lookups := len(kr.lookups)
	var out bytes.Buffer
	if err := Migrate(&out, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "nothing to migrate\n" || len(kr.lookups) != lookups {
		t.Fatal("idempotent migration performed new work")
	}
	if !reflect.DeepEqual(before, vaultTestState(t)) {
		t.Fatal("idempotent migration changed the vault")
	}
	requireMigrationUnchanged(t, manifest, m)
}

func TestMigrateAbsentOnly(t *testing.T) {
	kr := useMigrationKeyring(t)
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "anthropic")
	vaulttest.Put(t, vault.Env, "KEEP_ENV", "vault-env")
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"anthropic"}`)
	vaulttest.Put(t, vault.Overlay, "vendors", `{"vendors":{}}`)
	vaulttest.Secret(t, "KEEP_TOKEN", "vault-credential")
	kr.values["KEEP_TOKEN"] = "legacy-keyring-credential"
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\nHARNESS_OPUS_MODEL=new-model\n")
	seedLegacyFile(t, "custom.env", "KEEP_ENV=legacy-env\nNEW_ENV=new-env\n")
	seedLegacyFile(t, "providers.json", `{"default":"openai"}`)
	seedLegacyFile(t, "vendors.json", `{"vendors":{"copilot":null}}`)
	seedLegacyFile(t, "secrets.env", "KEEP_TOKEN=legacy-file-credential\nNEW_TOKEN=new-credential\n")
	before := decodeMigrationDoc(t)
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	after := decodeMigrationDoc(t)
	if after.Pins["HARNESS_PROVIDER"] != before.Pins["HARNESS_PROVIDER"] ||
		after.Env["KEEP_ENV"] != before.Env["KEEP_ENV"] ||
		!bytes.Equal(after.Providers, before.Providers) || !bytes.Equal(after.Vendors, before.Vendors) {
		t.Fatal("migration overwrote existing config entries")
	}
	if after.Pins["HARNESS_OPUS_MODEL"] != "new-model" || after.Env["NEW_ENV"] != "new-env" {
		t.Fatal("migration did not import absent config entries")
	}
	requireVaultSecret(t, "KEEP_TOKEN", "vault-credential", true)
	requireVaultSecret(t, "NEW_TOKEN", "new-credential", true)
	if slices.Contains(kr.lookups, "KEEP_TOKEN") {
		t.Fatal("migration looked up an already stored secret")
	}
	m := readMigrationManifest(t, 5)
	requireMigrationDone(t, m)
	for _, item := range m.Items {
		for _, name := range []string{"HARNESS_PROVIDER", "KEEP_ENV", "KEEP_TOKEN", "providers", "vendors"} {
			if slices.Contains(item.Names, name) {
				t.Errorf("manifest claims existing %s was imported", name)
			}
		}
	}
}

// R2: an `@keyring` reference with no secrets.env or secrets.meta.json file present must still be
// sealed into the vault, or migration must fail outright — a ref is never rewritten to `@secret`
// without a vault value backing it, which would strand the credential with no reader able to find it.
func TestMigrateKeyringOnlyReferenceSealsOrFails(t *testing.T) {
	kr := useMigrationKeyring(t)
	kr.values["TOKEN"] = "keyring-credential"
	source := seedLegacyFile(t, "custom.env", "TOKEN=@keyring\n")
	err := Migrate(io.Discard, false)
	if err != nil {
		requireMigrationFile(t, source, true)
		return
	}
	doc := decodeMigrationDoc(t)
	switch doc.Env["TOKEN"] {
	case "@secret":
		requireVaultSecret(t, "TOKEN", "keyring-credential", true)
	case "@keyring":
		// left untouched is also acceptable, as long as nothing was silently stranded
	default:
		t.Fatalf("unexpected TOKEN ref: %q", doc.Env["TOKEN"])
	}
}

func TestMigrateInvalidEnvDoesNotImportPartialFile(t *testing.T) {
	for _, tt := range []struct {
		name, content, errText string
	}{
		{"no equals", "BEFORE=one\nBROKEN\nAFTER=two\n", "line 2: want NAME=value"},
		{"truncated", "BEFORE=one\nLONG=" + strings.Repeat("x", 4<<20) + "\nAFTER=two\n", "read stopped early"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useMigrationKeyring(t)
			source := seedLegacyFile(t, "secrets.env", tt.content)
			err := Migrate(io.Discard, false)
			requireMigrationError(t, err, tt.errText)
			requireMigrationFile(t, source, true)
			requireMigrationFile(t, source+".bak", false)
			requireMigrationFile(t, manifestPath(), false)
			requireMigrationFile(t, vault.Path(), false)
			requireVaultSecret(t, "BEFORE", "", false)
			requireVaultSecret(t, "AFTER", "", false)
		})
	}
}

func TestMigrateKeyringFailurePartialAndRetry(t *testing.T) {
	for _, tt := range []struct {
		name      string
		err       error
		available bool
	}{
		{"unavailable", keyring.ErrUnavailable, true},
		{"no answer", keyring.ErrNoAnswer, true},
		{"no answer without tool", keyring.ErrNoAnswer, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			kr := useMigrationKeyring(t)
			kr.lookupErr, kr.available = tt.err, tt.available
			kr.values["REF_TOKEN"] = "reference-credential"
			seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
			seedLegacyFile(t, "custom.env", "REF_TOKEN=@keyring\n")
			seedLegacyFile(t, "providers.json", `{"default":"openai"}`)
			seedLegacyFile(t, "vendors.json", `{}`)
			source := seedLegacyFile(t, "secrets.env", "FILE_TOKEN=file-credential\n")
			meta := seedLegacyFile(t, "secrets.meta.json", `{"credentials":{}}`)
			requireMigrationError(t, Migrate(io.Discard, false),
				"keyring did not answer; re-run claude-code vault migrate")
			m := readMigrationManifest(t, 4)
			requireMigrationDone(t, m)
			requireMigrationFile(t, source, true)
			requireMigrationFile(t, meta, true)
			requireMigrationFile(t, source+".bak", false)
			requireMigrationFile(t, meta+".bak", false)
			requireVaultSecret(t, "FILE_TOKEN", "", false)
			if decodeMigrationDoc(t).Env["REF_TOKEN"] != "@secret" {
				t.Fatal("successful env group was not committed")
			}
			kr.lookupErr, kr.available = nil, true
			if err := Migrate(io.Discard, false); err != nil {
				t.Fatal(err)
			}
			requireMigrationDone(t, readMigrationManifest(t, 6))
			requireVaultSecret(t, "FILE_TOKEN", "file-credential", true)
			requireVaultSecret(t, "REF_TOKEN", kr.values["REF_TOKEN"], true)
		})
	}
}

func TestMigrateNoToolUsesFilePlane(t *testing.T) {
	for _, err := range []error{keyring.ErrNoTool, keyring.ErrUnavailable} {
		t.Run(err.Error(), func(t *testing.T) {
			kr := useMigrationKeyring(t)
			kr.available, kr.lookupErr = false, err
			seedLegacyFile(t, "secrets.env", "FILE_TOKEN=file-credential\n")
			if err := Migrate(io.Discard, false); err != nil {
				t.Fatal(err)
			}
			requireVaultSecret(t, "FILE_TOKEN", "file-credential", true)
			m := readMigrationManifest(t, 1)
			requireMigrationDone(t, m)
			if len(m.Keyring) != 0 {
				t.Fatal("no-tool migration claimed keyring credentials")
			}
		})
	}
}

func TestMigrateProvidersLocations(t *testing.T) {
	for _, both := range []bool{false, true} {
		name := "pre env.d"
		if both {
			name = "both locations"
		}
		t.Run(name, func(t *testing.T) {
			useMigrationKeyring(t)
			root := writeMigrationFile(t, rootProvidersFile(), `{"default":"openai"}`)
			var env string
			if both {
				env = seedLegacyFile(t, "providers.json", `{"default":"anthropic"}`)
			}
			err := Migrate(io.Discard, false)
			if both {
				requireMigrationError(t, err, "providers.json exists in both ")
				requireMigrationError(t, err, "and env.d; merge, then re-run")
				for _, path := range []string{root, env} {
					requireMigrationFile(t, path, true)
					requireMigrationFile(t, path+".bak", false)
				}
				requireMigrationFile(t, manifestPath(), false)
				if string(decodeMigrationDoc(t).Providers) != "null" {
					t.Fatal("conflicting providers were imported")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			m := readMigrationManifest(t, 1)
			requireMigrationDone(t, m)
			if m.Items[0].Source != root || !bytes.Contains(decodeMigrationDoc(t).Providers, []byte(`"openai"`)) {
				t.Fatal("pre-env.d provider source was not imported")
			}
		})
	}
}

func TestMigrateDryRunDoesNotWrite(t *testing.T) {
	useMigrationKeyring(t)
	source := seedLegacyFile(t, "secrets.env", "FILE_TOKEN=file-credential\n")
	var out bytes.Buffer
	if err := Migrate(&out, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would migrate 1 file(s)") {
		t.Fatal("dry run did not report the migration")
	}
	requireMigrationFile(t, source, true)
	requireMigrationFile(t, source+".bak", false)
	requireMigrationFile(t, source+".lock", false)
	requireMigrationFile(t, vault.Path(), false)
	requireMigrationFile(t, manifestPath(), false)
}

func TestMigrateResumesPendingRename(t *testing.T) {
	for _, renamed := range []bool{false, true} {
		name := "before rename"
		if renamed {
			name = "after rename before save"
		}
		t.Run(name, func(t *testing.T) {
			useMigrationKeyring(t)
			seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
			if err := Migrate(io.Discard, false); err != nil {
				t.Fatal(err)
			}
			m := readMigrationManifest(t, 1)
			if !renamed {
				if err := os.Rename(m.Items[0].Backup, m.Items[0].Source); err != nil {
					t.Fatal(err)
				}
			}
			m.Items[0].Done = false
			if err := m.save(); err != nil {
				t.Fatal(err)
			}
			before := vaultTestState(t)
			var out bytes.Buffer
			if err := Migrate(&out, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "resumed 1 rename(s)") {
				t.Fatal("pending rename was not resumed")
			}
			requireMigrationDone(t, readMigrationManifest(t, 1))
			if !reflect.DeepEqual(before, vaultTestState(t)) {
				t.Fatal("rename recovery reimported vault values")
			}
		})
	}
}

// R5: a crash after the manifest is saved but before finishRename's os.Rename, followed by the
// still-installed old binary updating that source, must not let resume silently rename the
// updated file and mark it done — its new content was never imported into the vault.
func TestMigrateResumeRefusesRenameOfChangedSource(t *testing.T) {
	useMigrationKeyring(t)
	seedLegacyFile(t, "provider.env", "HARNESS_PROVIDER=openai\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	m := readMigrationManifest(t, 1)
	// Undo the rename to put the source back, as if the crash happened before finishRename ran,
	// then let an old binary update the source after the manifest already recorded its old hash.
	if err := os.Rename(m.Items[0].Backup, m.Items[0].Source); err != nil {
		t.Fatal(err)
	}
	m.Items[0].Done = false
	if err := m.save(); err != nil {
		t.Fatal(err)
	}
	writeMigrationFile(t, m.Items[0].Source, "HARNESS_PROVIDER=openai\nHARNESS_OPUS_MODEL=updated-by-old-binary\n")
	err := Migrate(io.Discard, false)
	if err == nil {
		t.Fatal("expected resume to refuse renaming a source changed since migration")
	}
	requireMigrationFile(t, m.Items[0].Source, true)
	requireMigrationFile(t, m.Items[0].Backup, false)
	if string(readMigrationFile(t, m.Items[0].Source)) !=
		"HARNESS_PROVIDER=openai\nHARNESS_OPUS_MODEL=updated-by-old-binary\n" {
		t.Fatal("refused resume must not touch the changed source")
	}
}

func TestMigrateDeleteThenRerunDoesNotResurrect(t *testing.T) {
	kr := useMigrationKeyring(t)
	kr.values["FILE_TOKEN"] = "retained-keyring-credential"
	seedLegacyFile(t, "secrets.env", "FILE_TOKEN=file-credential\n")
	if err := Migrate(io.Discard, false); err != nil {
		t.Fatal(err)
	}
	requireVaultSecret(t, "FILE_TOKEN", kr.values["FILE_TOKEN"], true)
	if err := secrets.Delete("FILE_TOKEN"); err != nil {
		t.Fatal(err)
	}
	requireVaultSecret(t, "FILE_TOKEN", "", false)
	lookups := len(kr.lookups)
	var out bytes.Buffer
	if err := Migrate(&out, false); err != nil {
		t.Fatal(err)
	}
	if out.String() != "nothing to migrate\n" || len(kr.lookups) != lookups {
		t.Fatal("source-free rerun attempted a keyring import")
	}
	requireVaultSecret(t, "FILE_TOKEN", "", false)
	requireMigrationDone(t, readMigrationManifest(t, 1))
}
