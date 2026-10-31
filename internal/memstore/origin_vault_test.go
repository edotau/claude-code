package memory

import (
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

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
// disk, and ReadOrigin returns it byte-for-byte (aliases included).
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
	got, err := ReadOrigin(dir)
	if err != nil || !reflect.DeepEqual(got, o) {
		t.Fatalf("ReadOrigin = %+v, %v, want %+v", got, err, o)
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
	if got, err := ReadOrigin(dir); err != nil || got.Origin != o.Origin {
		t.Fatalf("ReadOrigin without a master key = %+v, %v", got, err)
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

	if _, err := ReadOrigin(dir); err == nil {
		t.Fatal("ReadOrigin imported a legacy .origin file")
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
	if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: "one", Origin: "/src/other"}); !errors.Is(err, ErrOriginConflict) {
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
// carries the origin comment on this key's catalog row — ReadOrigin rebuilds the record from it
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
	catalogPath := filepath.Join(cfg, "memory", CatalogFile)
	write(t, catalogPath, "# Memory catalog\n\n## Repos\n\n"+row)
	before := readFile(t, catalogPath)

	got, err := ReadOrigin(dir)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadOrigin (catalog fallback) = %+v, %v, want %+v", got, err, want)
	}
	if after := readFile(t, catalogPath); after != before {
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
	write(t, filepath.Join(cfg, "memory", CatalogFile), "# Memory catalog\n\n## Repos\n\n"+row)

	got := catalogOrigins()["weird"]
	if got.Origin != want.Origin || len(got.Aliases) != 1 || got.Aliases[0] != want.Aliases[0] {
		t.Errorf("catalogOrigins round trip = %+v, want %+v", got, want)
	}
}

// TestBuildCatalogRebuildsIdenticalRowsAfterVaultLoss: BuildCatalog's rows (with their origin
// comments) are the sole durable record once the vault is gone — deleting vault.db and asking
// RepoDirs again must reproduce exactly the same origins from the catalog it wrote.
func TestBuildCatalogRebuildsIdenticalRowsAfterVaultLoss(t *testing.T) {
	resetVaultRetry(t)
	cfg := tempConfig(t)
	for _, k := range []string{"alpha", "beta"} {
		dir := filepath.Join(cfg, "memory", k)
		os.MkdirAll(dir, 0o755)
		if err := WriteOrigin(dir, Origin{Version: OriginVersion, Key: k, Origin: "/src/" + k, Aliases: []string{"/alt-" + k}}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := RepoDirs()
	if err != nil || len(before) != 2 {
		t.Fatalf("RepoDirs before = %+v, %v", before, err)
	}
	catalog, err := BuildCatalog(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(cfg, "memory", CatalogFile), catalog)

	if err := os.Remove(vault.Path()); err != nil {
		t.Fatal(err)
	}
	after, err := RepoDirs()
	if err != nil || len(after) != 2 {
		t.Fatalf("RepoDirs after vault loss = %+v, %v", after, err)
	}
	for i := range before {
		if !reflect.DeepEqual(before[i].Origin, after[i].Origin) {
			t.Errorf("entry %d origin changed: before %+v, after %+v", i, before[i].Origin, after[i].Origin)
		}
	}
}

// TestVaultLockedFallsBackToCatalogThenSkipsRetryFor30s: an external LOCK_EX on vault.db (a real OS
// file lock, not just an in-process transaction) still lets RepoDirs resolve from index.md, bounded
// by the vault.View lock timeout (~2s) rather than hanging; it writes nothing back while locked, and
// a second call made promptly after does not pay that wait again (the 30s read-skip backoff).
func TestVaultLockedFallsBackToCatalogThenSkipsRetryFor30s(t *testing.T) {
	resetVaultRetry(t)
	t.Cleanup(vault.SetReadLockTimeoutForTest(100 * time.Millisecond))
	cfg := tempConfig(t)
	dir := filepath.Join(cfg, "memory", "locked")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := Origin{Version: OriginVersion, Key: "locked", Origin: "/src/locked"}
	row := "- locked — hook · last 2024-01-01 → locked/MEMORY.md" + catalogOriginComment(want) + "\n"
	write(t, filepath.Join(cfg, "memory", CatalogFile), "# Memory catalog\n\n## Repos\n\n"+row)

	// Force the vault file into existence before taking an external flock on it.
	if err := vault.Update(func(tx *vault.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(vault.Path(), syscall.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	entries, err := RepoDirs()
	elapsed := time.Since(start)
	if err != nil || len(entries) != 1 || entries[0].Origin.Origin != want.Origin {
		t.Fatalf("RepoDirs under an external lock = %+v, %v, want the locked entry", entries, err)
	}
	if elapsed > 2500*time.Millisecond {
		t.Errorf("RepoDirs under lock took %v, want bounded by the ~2s vault.View timeout", elapsed)
	}

	start2 := time.Now()
	if _, err := RepoDirs(); err != nil {
		t.Fatal(err)
	}
	if e2 := time.Since(start2); e2 > time.Second {
		t.Errorf("second call within the 30s backoff took %v, want near-instant (no repeat wait)", e2)
	}

	// Only now release the external lock: any vault.View while it is held would itself block on the
	// same timeout, so the no-write assertion below must run after releasing, not while testing the fallback.
	if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if _, ok := vaultGet(t, "locked"); ok {
		t.Error("a locked vault.View must never be followed by a write attempt")
	}
}

// TestAliasFoldDeletesSourceVaultRow: AddOriginAlias-style folding (foldOrigin's drop path) must
// delete the source store's own vault row in the same transaction that gives the target the alias —
// never leave a stale row behind for a dir that no longer exists.
func TestAliasFoldDeletesSourceVaultRow(t *testing.T) {
	resetVaultRetry(t)
	e := newAliasEnv(t)
	if _, err := alias(t, e, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := vaultGet(t, e.kb); ok {
		t.Errorf("source key %q still has a vault row after folding into %q", e.kb, e.ka)
	}
	got, err := ReadOrigin(e.da)
	if err != nil || !got.Accepts(e.b) {
		t.Errorf("target row missing the folded alias: %+v, %v", got, err)
	}
}

// vaultGet is a test-only direct peek at a memory_origins row, bypassing resolveOrigins' fallbacks.
func vaultGet(t *testing.T, key string) (string, bool) {
	t.Helper()
	var raw string
	var ok bool
	if err := vault.View(func(tx *vault.Tx) error { raw, ok = tx.Get(vault.MemoryOrigins, key); return nil }); err != nil {
		t.Fatal(err)
	}
	return raw, ok
}

var _ = strings.TrimSpace // keep strings imported if unused helpers are trimmed later
