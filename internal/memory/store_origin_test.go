package memory

import (
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

// resetVaultRetry clears the process-wide vault-read backoff (origin.go's vaultRetryAt) so a test
// that deliberately forces a vault-lock timeout cannot poison every test that runs after it in the
// same binary; call it both before (clean start) and after (cleanup) any such test.
func resetVaultRetry(t *testing.T) {
	t.Helper()
	vaultRetryAt.Store(0)
	t.Cleanup(func() { vaultRetryAt.Store(0) })
}

// TestOriginRoundTripsThroughVault: WriteOrigin lands a row in the vault, never a legacy file on
// disk, and readOrigin returns it byte-for-byte (aliases included).
func TestOriginRoundTripsThroughVault(t *testing.T) {
	resetVaultRetry(t)
	cfg := tempConfig(t)
	dir := filepath.Join(cfg, "memory", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	o := Origin{Version: OriginVersion, Key: "proj", Origin: "/src/proj", Aliases: []string{"/alt"}}
	if err := WriteOrigin(dir, o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, legacyOriginFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("WriteOrigin left a legacy file on disk: %v", err)
	}
	if raw, ok := vaultGet(t, "proj"); !ok || raw == "" {
		t.Fatal("WriteOrigin did not write a memory_origins row")
	}
	got, err := readOrigin(dir)
	if err != nil || !reflect.DeepEqual(got, o) {
		t.Fatalf("readOrigin = %+v, %v, want %+v", got, err, o)
	}
}

// TestReadOriginWorksWithoutMasterKey: memory_origins is plaintext — no vault master key (keyring,
// HARNESS_VAULT_KEY or key_command) is needed to write or read a row.
func TestReadOriginWorksWithoutMasterKey(t *testing.T) {
	resetVaultRetry(t)
	cfg := tempConfig(t)
	t.Setenv("HARNESS_VAULT_KEY", "")
	os.Unsetenv("HARNESS_VAULT_KEY")
	dir := filepath.Join(cfg, "memory", "nokey")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	o := Origin{Version: OriginVersion, Key: "nokey", Origin: "/src/nokey"}
	if err := WriteOrigin(dir, o); err != nil {
		t.Fatalf("WriteOrigin without a master key: %v", err)
	}
	if got, err := readOrigin(dir); err != nil || got.Origin != o.Origin {
		t.Fatalf("readOrigin without a master key = %+v, %v", got, err)
	}
}

// TestRepoDirsReadsOriginsWithSealedSecretsButNoMasterKey: a vault that also holds sealed secrets
// (needing a master key to decrypt) must still serve memory_origins — a plaintext bucket — to a
// caller with no master key at all.
func TestRepoDirsReadsOriginsWithSealedSecretsButNoMasterKey(t *testing.T) {
	resetVaultRetry(t)
	cfg := tempConfig(t)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	dir := filepath.Join(cfg, "memory", "sealed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	o := Origin{Version: OriginVersion, Key: "sealed", Origin: "/src/sealed"}
	if err := WriteOrigin(dir, o); err != nil {
		t.Fatal(err)
	}
	if err := vault.UpdateSealed(func(tx *vault.Tx, k *vault.Key) error {
		return tx.PutRecord("SOME_SECRET", vault.Record{Sealed: k.Seal("SOME_SECRET", "s3cr3t"), Kid: k.Kid})
	}); err != nil {
		t.Fatalf("seed sealed secret: %v", err)
	}

	t.Setenv("HARNESS_VAULT_KEY", "")
	os.Unsetenv("HARNESS_VAULT_KEY")
	entries, err := RepoDirs()
	if err != nil {
		t.Fatalf("RepoDirs with sealed secrets present and no master key: %v", err)
	}
	if len(entries) != 1 || entries[0].Key != "sealed" {
		t.Errorf("RepoDirs = %+v, want exactly the sealed-vault entry", entries)
	}
}

// TestLegacyOriginFileIgnored: a pre-vault .origin file is neither imported nor removed; the key has no origin.
func TestLegacyOriginFileIgnored(t *testing.T) {
	resetVaultRetry(t)
	cfg := tempConfig(t)
	dir := filepath.Join(cfg, "memory", "legacy")
	legacyPath := filepath.Join(dir, legacyOriginFile)
	write(t, legacyPath, `{"version":1,"key":"legacy","origin":"/src/legacy"}`)

	if _, err := readOrigin(dir); err == nil {
		t.Fatal("readOrigin imported a legacy .origin file")
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Errorf("legacy file was touched: %v", err)
	}
	if _, ok := vaultGet(t, "legacy"); ok {
		t.Error("legacy file was folded into a memory_origins row")
	}
}

// TestWriteOriginGuards: every disqualifying shape WriteOrigin must refuse, each for its own reason.
func TestWriteOriginGuards(t *testing.T) {
	resetVaultRetry(t)
	cfg := tempConfig(t)
	dir := filepath.Join(cfg, "memory", "one")
	os.MkdirAll(dir, 0o755)
	o := Origin{Version: OriginVersion, Key: "one", Origin: "/src/one"}
	if err := WriteOrigin(dir, o); err != nil {
		t.Fatal(err)
	}
	if err := WriteOrigin(dir, o); err != nil {
		t.Error("same identity must be a no-op:", err)
	}
	if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: "one", Origin: "/src/other"}); !errors.Is(err, errOriginConflict) {
		t.Errorf("different origin must conflict, got %v", err)
	}
	if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: "two", Origin: "/src/one"}); err == nil {
		t.Error("key must match the directory")
	}
	link := filepath.Join(cfg, "memory", "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip(err)
	}
	if err := WriteOrigin(link, Origin{Version: OriginVersion, Key: "link", Origin: "/x"}); err == nil {
		t.Error("a symlinked store must be rejected")
	}
	nested := filepath.Join(dir, "nested")
	os.MkdirAll(nested, 0o755)
	if err := WriteOrigin(nested, Origin{Version: OriginVersion, Key: "nested", Origin: "/x"}); err == nil {
		t.Error("a dir not a direct child of the store root must be rejected")
	}
}

// TestOriginRebuiltFromCatalogWhenRowAndFileAbsent: no vault row, no legacy file, but index.md
// carries the origin comment on this key's catalog row — readOrigin rebuilds the record from it
// (aliases included), and the catalog file itself is left byte-for-byte unchanged.
func TestOriginRebuiltFromCatalogWhenRowAndFileAbsent(t *testing.T) {
	resetVaultRetry(t)
	cfg := tempConfig(t)
	dir := filepath.Join(cfg, "memory", "catkey")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := Origin{Version: OriginVersion, Key: "catkey", Origin: "/src/cat", Aliases: []string{"/alt1", "/alt2"}}
	row := "- catkey — some-hook · last 2024-01-01 → catkey/MEMORY.md" + catalogOriginComment(want) + "\n"
	catalogPath := filepath.Join(cfg, "memory", catalogFile)
	write(t, catalogPath, "# Memory catalog\n\n## Repos\n\n"+row)
	before := mustRead(t, catalogPath)

	got, err := readOrigin(dir)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("readOrigin (catalog fallback) = %+v, %v, want %+v", got, err, want)
	}
	if after := mustRead(t, catalogPath); after != before {
		t.Errorf("catalog rewritten by the fallback import:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if raw, ok := vaultGet(t, "catkey"); !ok || raw == "" {
		t.Error("catalog-sourced origin was not rebuilt into a memory_origins row")
	}
}

// TestCatalogOriginCommentRoundTripsPathWithCommentCloser: an origin path containing the literal
// bytes "-->" must never be able to close the HTML comment early — json.Marshal escapes '>', so the
// round trip through catalogOriginComment/catalogOrigins must reproduce it exactly.
func TestCatalogOriginCommentRoundTripsPathWithCommentCloser(t *testing.T) {
	resetVaultRetry(t)
	cfg := tempConfig(t)
	want := Origin{Version: OriginVersion, Key: "weird", Origin: "/src/a-->b", Aliases: []string{"/alt-->c"}}
	row := "- weird — hook · last never → weird/MEMORY.md" + catalogOriginComment(want) + "\n"
	write(t, filepath.Join(cfg, "memory", catalogFile), "# Memory catalog\n\n## Repos\n\n"+row)

	got := catalogOrigins()["weird"]
	if got.Origin != want.Origin || len(got.Aliases) != 1 || got.Aliases[0] != want.Aliases[0] {
		t.Errorf("catalogOrigins round trip = %+v, want %+v", got, want)
	}
}
