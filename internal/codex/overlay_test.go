package codex_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestConfigHistoryIsEncryptedBoundedAndSurvivesRotation(t *testing.T) {
	dir, _ := vaulttest.UseKeyring(t)
	path := filepath.Join(dir, "native", "config.toml")
	other := filepath.Join(dir, "other", "config.toml")
	fixture := []byte("private-config-fixture-68b19d\nforeign = 'preserve exactly'\n")
	seedCodexConfigHistory(t, path, other, fixture)
	assertCodexConfigHistoryEncrypted(t, dir, fixture)
	oldKid, newKid, err := vault.RotateKey()
	if err != nil || oldKid == newKid {
		t.Fatalf("rotation did not change the key: %v", err)
	}
	assertCodexRotatedContents(t, path, fixture)
	assertCodexRotatedMetadata(t, newKid)
	consumeCodexHistoryVersions(t, path)
	if _, found, err := codex.LatestCodexBackup(path); err != nil || found {
		t.Fatalf("more than five snapshots retained: found=%v, err=%v", found, err)
	}
	assertCodexHistoryOtherAndBaseline(t, path, other, fixture)
}

func seedCodexConfigHistory(t *testing.T, path, other string, fixture []byte) {
	t.Helper()
	if err := codex.SaveCodexBaseline(path, fixture); err != nil {
		t.Fatal(err)
	}
	for i := range 7 {
		if err := codex.CodexBackup(path, fmt.Appendf(nil, "version %d", i), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := codex.CodexBackup(other, fixture, false); err != nil {
		t.Fatal(err)
	}
	vaulttest.Secret(t, "API_KEY", "credential-fixture")
}

func assertCodexConfigHistoryEncrypted(t *testing.T, dir string, fixture []byte) {
	t.Helper()
	pairs, err := vault.ExportPairs(nil)
	if err != nil || !reflect.DeepEqual(pairs, []vault.Pair{{Name: "API_KEY", Value: "credential-fixture"}}) {
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
}

func assertCodexRotatedContents(t *testing.T, path string, fixture []byte) {
	t.Helper()
	baseline, err := codex.LoadCodexBaseline(path)
	if err != nil || !bytes.Equal(baseline, fixture) {
		t.Fatalf("baseline did not survive rotation: %v", err)
	}
	pairs, err := vault.ExportPairs(nil)
	if err != nil || !reflect.DeepEqual(pairs, []vault.Pair{{Name: "API_KEY", Value: "credential-fixture"}}) {
		t.Fatalf("credential did not survive rotation: %v", err)
	}
}

func assertCodexRotatedMetadata(t *testing.T, newKid string) {
	t.Helper()
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
}

func consumeCodexHistoryVersions(t *testing.T, path string) {
	t.Helper()
	for i := 6; i >= 2; i-- {
		b, found, err := codex.LatestCodexBackup(path)
		if err != nil || !found || string(b.Data) != fmt.Sprintf("version %d", i) || !b.Existed || b.Path != path {
			t.Fatalf("latest backup %d: found=%v, data=%q, err=%v", i, found, b.Data, err)
		}
		if err := codex.ConsumeCodexBackup(b.Name); err != nil {
			t.Fatal(err)
		}
	}
}

func assertCodexHistoryOtherAndBaseline(t *testing.T, path, other string, fixture []byte) {
	t.Helper()
	b, found, err := codex.LatestCodexBackup(other)
	if err != nil || !found || !bytes.Equal(b.Data, fixture) || b.Existed || b.Path != other {
		t.Fatalf("other path snapshot changed: found=%v, err=%v", found, err)
	}
	baseline, err := codex.LoadCodexBaseline(path)
	if err != nil || !bytes.Equal(baseline, fixture) {
		t.Fatalf("snapshot consumption changed baseline: %v", err)
	}
}

func TestCredentialAndConfigCiphertextCannotBeExchanged(t *testing.T) {
	vaulttest.UseKeyring(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := codex.CodexBackup(path, []byte("original native config"), true); err != nil {
		t.Fatal(err)
	}
	b, found, err := codex.LatestCodexBackup(path)
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

// codex.MergeCodexTrust must not mistake a "["-led line inside an open array for a table header: an
// existing trusted_hash AFTER the array, still inside the same table, is replaced in place —
// not left stale with a duplicate appended at EOF because the array wrongly reset the active table.
func TestMergeCodexTrustBracketInArrayNotHeader(t *testing.T) {
	raw := "[hooks.state.\"K\"]\nextra = [\n  [1, 2],\n]\ntrusted_hash = \"old\"\n"
	entries := map[string]string{"K": "abc123"}
	got := codex.MergeCodexTrust(raw, entries)
	want := "[hooks.state.\"K\"]\nextra = [\n  [1, 2],\n]\ntrusted_hash = \"abc123\"\n"
	if got != want {
		t.Fatalf("codex.MergeCodexTrust:\n%q\nwant:\n%q", got, want)
	}
	if strings.Count(got, "trusted_hash") != 1 {
		t.Errorf("want exactly one trusted_hash line, got:\n%q", got)
	}
}

// A real table header following a "["-led array element must still pick up its own hash, proving
// the depth tracking resets once the array closes rather than staying stuck open.
func TestMergeCodexTrustHeaderAfterArrayStillMatches(t *testing.T) {
	raw := "extra = [\n  [1, 2],\n]\n[hooks.state.\"K2\"]\nother = 1\n"
	entries := map[string]string{"K2": "def456"}
	got := codex.MergeCodexTrust(raw, entries)
	want := "extra = [\n  [1, 2],\n]\n[hooks.state.\"K2\"]\nother = 1\ntrusted_hash = \"def456\"\n"
	if got != want {
		t.Fatalf("codex.MergeCodexTrust:\n%q\nwant:\n%q", got, want)
	}
}

func TestMergeCodexTrustPreservesUnownedText(t *testing.T) {
	raw := `note = """
[hooks.state."active"]
trusted_hash = "inside string"
"""
[hooks.state.'active'] # keep table comment
  'trusted_hash' = 'old' # keep hash comment
other = 1
[hooks.state."missing"]
other = 2
[foreign]
trusted_hash = "unowned"`
	want := `note = """
[hooks.state."active"]
trusted_hash = "inside string"
"""
[hooks.state.'active'] # keep table comment
  'trusted_hash' = "new" # keep hash comment
other = 1
[hooks.state."missing"]
other = 2
trusted_hash = "added"
[foreign]
trusted_hash = "unowned"

[hooks.state."a"]
trusted_hash = "first"

[hooks.state."z"]
trusted_hash = "last"
`
	entries := map[string]string{"active": "new", "missing": "added", "z": "last", "a": "first"}
	got := codex.MergeCodexTrust(raw, entries)
	if got != want {
		t.Fatalf("codex.MergeCodexTrust:\n%s\nwant:\n%s", got, want)
	}
	if again := codex.MergeCodexTrust(got, entries); again != got {
		t.Fatalf("merge is not idempotent:\n%s", again)
	}
	if len(entries) != 4 {
		t.Fatal("merge mutated input entries")
	}
}

func TestOverlayEffortAcceptsMinimalAndRejectsMax(t *testing.T) {
	if err := codex.ValidateCodexOverlay(map[string]string{"model_reasoning_effort": "minimal"}); err != nil {
		t.Errorf("minimal rejected: %v", err)
	}
	if err := codex.ValidateCodexOverlay(map[string]string{"model_reasoning_effort": "max"}); err == nil {
		t.Error("max accepted")
	}
}
