package vault_test

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func specMasterKey(t *testing.T) *vault.Key {
	t.Helper()
	// Materialize the vault (and its real vault_id) before resolving a key to seal/open with:
	// the HKDF subkey is salted by vault_id, so a key derived against a not-yet-created vault's
	// empty id would mismatch the id the first write actually persists.
	if err := vault.Update(func(tx *vault.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	key, source, err := vault.MasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if key == nil || source != "env" {
		t.Fatalf("MasterKey = %v, %q; want non-nil env key", key, source)
	}
	return key
}

func TestSealOpenRoundTrip(t *testing.T) {
	vaulttest.Use(t)
	key := specMasterKey(t)
	for _, value := range []string{"", "representative-token", "line one\nline two\x00end"} {
		t.Run(fmt.Sprintf("bytes_%d", len(value)), func(t *testing.T) {
			sealed := key.Seal("TOKEN_A", value)
			if len(sealed) != len(value)+28 {
				t.Errorf("sealed length = %d, want %d (96-bit nonce + GCM tag)", len(sealed), len(value)+28)
			}
			got, err := key.Open("TOKEN_A", vault.Record{Sealed: sealed, Kid: key.Kid})
			if err != nil || got != value {
				t.Fatalf("Open round trip = %q, %v; want %q, nil", got, err, value)
			}
		})
	}
	vaulttest.Secret(t, "STORED_TOKEN", "stored-round-trip")
	if err := vault.View(func(tx *vault.Tx) error {
		record, ok, rerr := tx.Record("STORED_TOKEN")
		if rerr != nil {
			return rerr
		}
		if !ok {
			t.Fatal("vaulttest.Secret did not persist the record")
		}
		got, err := key.Open("STORED_TOKEN", record)
		if err != nil || got != "stored-round-trip" {
			t.Errorf("stored round trip = %q, %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSealOpenRejectsAADSwap(t *testing.T) {
	vaulttest.Use(t)
	key := specMasterKey(t)
	record := vault.Record{Sealed: key.Seal("TOKEN_A", "bound-to-A"), Kid: key.Kid}
	if got, err := key.Open("TOKEN_A", record); err != nil || got != "bound-to-A" {
		t.Fatalf("same-name control failed: got=%q err=%v", got, err)
	}
	if got, err := key.Open("TOKEN_B", record); err == nil {
		t.Fatalf("AAD swap accepted record sealed for TOKEN_A as TOKEN_B: got=%q", got)
	}
	aead := specSecretsAEAD(t)
	got, err := aead.Open(nil, nil, record.Sealed, []byte("claude-code/vault/v1/secrets/TOKEN_A"))
	if err != nil || string(got) != "bound-to-A" {
		t.Fatalf("ciphertext is not bound to the specified AAD prefix and name: got=%q err=%v", got, err)
	}
}

// specSecretsAEAD derives the secrets AEAD from HARNESS_VAULT_KEY and vault_id per the spec, not via package code.
func specSecretsAEAD(t *testing.T) cipher.AEAD {
	t.Helper()
	rawKey, err := base64.StdEncoding.DecodeString(os.Getenv("HARNESS_VAULT_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	var vaultID string
	if err := vault.View(func(tx *vault.Tx) error {
		vaultID, _ = tx.Get(vault.Meta, "vault_id")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sub, err := hkdf.Key(sha256.New, rawKey, []byte(vaultID), "claude-code/vault/v1/secrets", 32)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(sub)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		t.Fatal(err)
	}
	return aead
}

func TestSealOpenWrongKid(t *testing.T) {
	vaulttest.Use(t)
	key := specMasterKey(t)
	record := vault.Record{Sealed: key.Seal("TOKEN_A", "value"), Kid: "not-" + key.Kid}
	if _, err := key.Open("TOKEN_A", record); !errors.Is(err, vault.ErrWrongKey) {
		t.Fatalf("Open with wrong kid = %v, want ErrWrongKey", err)
	}
}

func TestViewMissingVaultIsEmptyWithoutCreatingFile(t *testing.T) {
	dir := vaulttest.Use(t)
	wantPath := filepath.Join(dir, "state", "vault.db")
	if got := vault.Path(); got != wantPath {
		t.Fatalf("Path = %q, want %q", got, wantPath)
	}
	called := false
	if err := vault.View(func(tx *vault.Tx) error {
		called = true
		checkEmptyTx(t, tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("View did not call the callback on a missing vault")
	}
	if _, err := os.Stat(wantPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("View created a vault or stat failed unexpectedly: %v", err)
	}
}

// checkEmptyBucket asserts bucket has no "absent" key, no rows and no names.
func checkEmptyBucket(t *testing.T, tx *vault.Tx, bucket string) {
	t.Helper()
	if value, ok := tx.Get(bucket, "absent"); ok || value != "" {
		t.Errorf("missing %s Get = %q, %t", bucket, value, ok)
	}
	if len(tx.All(bucket)) != 0 || len(tx.Names(bucket)) != 0 {
		t.Errorf("missing %s is not empty", bucket)
	}
}

// checkEmptyTx asserts every bucket read on tx is empty and no record decodes.
func checkEmptyTx(t *testing.T, tx *vault.Tx) {
	t.Helper()
	for _, bucket := range []string{vault.Pins, vault.Env, vault.Overlay, vault.Secrets, vault.Meta} {
		checkEmptyBucket(t, tx, bucket)
	}
	if record, ok, err := tx.Record("absent"); ok || err != nil || !reflect.DeepEqual(record, vault.Record{}) {
		t.Errorf("missing Record = %+v, %t, %v", record, ok, err)
	}
	if recs, err := tx.Records(); err != nil || len(recs) != 0 {
		t.Errorf("missing vault has records or errored: %v, %v", recs, err)
	}
}

func TestConcurrentUpdatePreservesEightKeys(t *testing.T) {
	vaulttest.Use(t)
	const writers = 8
	start := make(chan struct{})
	results := make(chan error, writers)
	for i := range writers {
		go func(i int) {
			<-start
			results <- vault.Update(func(tx *vault.Tx) error {
				return tx.Put(vault.Pins, fmt.Sprintf("key-%d", i), fmt.Sprintf("value-%d", i))
			})
		}(i)
	}
	close(start)
	for range writers {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("concurrent Update: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent Update did not finish within writer timeout")
		}
	}
	checkEightPins(t, writers)
}

// checkEightPins asserts pins holds exactly key-i=value-i for every writer i.
func checkEightPins(t *testing.T, writers int) {
	t.Helper()
	if err := vault.View(func(tx *vault.Tx) error {
		all := tx.All(vault.Pins)
		if len(all) != writers {
			t.Errorf("got %d keys, want %d: %v", len(all), writers, all)
		}
		for i := range writers {
			name, want := fmt.Sprintf("key-%d", i), fmt.Sprintf("value-%d", i)
			if got, ok := tx.Get(vault.Pins, name); !ok || got != want {
				t.Errorf("Get(%q) = %q, %t; want %q, true", name, got, ok, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestViewWaitsForHeldUpdate(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Pins, "held", "before")
	held, release := make(chan struct{}), make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- vault.Update(func(tx *vault.Tx) error {
			close(held)
			<-release
			return tx.Put(vault.Pins, "held", "after")
		})
	}()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	select {
	case <-held:
	case err := <-writeDone:
		t.Fatalf("Update ended before acquiring transaction: %v", err)
	case <-time.After(6 * time.Second):
		t.Fatal("Update never acquired the write transaction")
	}
	readDone := startHeldReader()
	select {
	case err := <-readDone:
		t.Fatalf("View returned while write transaction was held: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	released = true
	awaitAll(t, map[string]<-chan error{"writer": writeDone, "reader": readDone})
}

// startHeldReader starts a View that must see the held write's committed "after", returning once it is running.
func startHeldReader() <-chan error {
	readStarted, readDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(readStarted)
		readDone <- vault.View(func(tx *vault.Tx) error {
			if got, ok := tx.Get(vault.Pins, "held"); !ok || got != "after" {
				return fmt.Errorf("reader saw %q, %t; want committed value after, true", got, ok)
			}
			return nil
		})
	}()
	<-readStarted
	return readDone
}

// awaitAll waits up to 6s for each named done channel and reports any error or timeout.
func awaitAll(t *testing.T, dones map[string]<-chan error) {
	t.Helper()
	for name, done := range dones {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s failed after release: %v", name, err)
			}
		case <-time.After(6 * time.Second):
			t.Errorf("%s did not finish after release", name)
		}
	}
}

func TestViewRejectsNewerSchema(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Meta, "schema", "999")
	err := vault.View(func(tx *vault.Tx) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "newer claude-code") {
		t.Fatalf("View schema 3 = %v, want error containing newer claude-code", err)
	}
}

// rawKeyAndKid builds a deterministic 32-byte master key (every byte == b) and its derived kid,
// matching parseKey's sha256(root)[:16] hex scheme, without reaching into the vault package.
func rawKeyAndKid(b byte) (b64, kid string) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = b
	}
	b64 = base64.StdEncoding.EncodeToString(raw)
	sum := sha256.Sum256(raw)
	kid = hex.EncodeToString(sum[:])[:16]
	return
}

// writeKeyCommandScript writes an executable that prints value to stdout, and returns meta/key_command's JSON argv.
func writeKeyCommandScript(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key_command.sh")
	script := "#!/bin/sh\nprintf '%s' '" + value + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	argv, err := json.Marshal([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	return string(argv)
}

// writeFailingKeyCommandScript writes an executable that writes msg to stderr and exits non-zero.
func writeFailingKeyCommandScript(t *testing.T, msg string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key_command_fail.sh")
	script := "#!/bin/sh\necho '" + msg + "' >&2\nexit 7\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	argv, err := json.Marshal([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	return string(argv)
}

// TestResolveKeyEnvMismatchFallsThroughToWorkingKeyCommand: HARNESS_VAULT_KEY set to a key whose
// kid does not match the vault's recorded kid must not hard-fail when a key_command can supply
// the right one — the mismatched env key is just one candidate source, not the final word.
func TestResolveKeyEnvMismatchFallsThroughToWorkingKeyCommand(t *testing.T) {
	vaulttest.UseKeyring(t)
	goodB64, goodKid := rawKeyAndKid(0x11)
	_, badKid := rawKeyAndKid(0x22)
	if goodKid == badKid {
		t.Fatal("fixture kids collide")
	}

	// Stamp meta/kid = goodKid by actually sealing a record under the good key via the real path.
	t.Setenv("HARNESS_VAULT_KEY", goodB64)
	if err := vault.UpdateSealed(func(tx *vault.Tx, k *vault.Key) error {
		if k.Kid != goodKid {
			t.Fatalf("fixture key kid = %s, want %s", k.Kid, goodKid)
		}
		return tx.PutRecord("NAME", vault.Record{Sealed: k.Seal("NAME", "secret-value"), Kid: k.Kid})
	}); err != nil {
		t.Fatal(err)
	}

	// Now HARNESS_VAULT_KEY mismatches meta/kid, but key_command resolves the matching key.
	badB64, _ := rawKeyAndKid(0x22)
	t.Setenv("HARNESS_VAULT_KEY", badB64)
	vaulttest.Put(t, vault.Meta, "key_command", writeKeyCommandScript(t, goodB64))

	value, found, err := vault.ReadSecret("NAME")
	if err != nil {
		t.Fatalf("ReadSecret with a mismatched env key but a working key_command: %v", err)
	}
	if !found || value != "secret-value" {
		t.Fatalf("ReadSecret = (%q, %v), want (secret-value, true)", value, found)
	}

	key, source, err := vault.MasterKey()
	if err != nil {
		t.Fatalf("MasterKey: %v", err)
	}
	if source != "command" || key.Kid != goodKid {
		t.Errorf("MasterKey resolved via %q kid %q, want command/%s", source, key.Kid, goodKid)
	}
}

// TestResolveKeyEnvMismatchNoFallbackErrorsWrongKey: with no key_command and nothing in the
// keyring for the recorded kid, a mismatched HARNESS_VAULT_KEY must still fail, wrapping
// ErrWrongKey rather than any raw keyring/env error.
func TestResolveKeyEnvMismatchNoFallbackErrorsWrongKey(t *testing.T) {
	vaulttest.UseKeyring(t)
	goodB64, goodKid := rawKeyAndKid(0x33)
	badB64, badKid := rawKeyAndKid(0x44)
	if goodKid == badKid {
		t.Fatal("fixture kids collide")
	}

	t.Setenv("HARNESS_VAULT_KEY", goodB64)
	if err := vault.UpdateSealed(func(tx *vault.Tx, k *vault.Key) error {
		return tx.PutRecord("NAME", vault.Record{Sealed: k.Seal("NAME", "secret-value"), Kid: k.Kid})
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HARNESS_VAULT_KEY", badB64)
	_, _, err := vault.MasterKey()
	if err == nil {
		t.Fatal("MasterKey succeeded with a mismatched env key and no fallback")
	}
	if !errors.Is(err, vault.ErrWrongKey) {
		t.Errorf("MasterKey error = %v, want errors.Is ErrWrongKey", err)
	}
}

// TestKeyCommandFailureIncludesStderr: a failing key_command's error must carry the command's
// stderr text, not just the bare exit-status error, so a misconfigured command is diagnosable.
func TestKeyCommandFailureIncludesStderr(t *testing.T) {
	vaulttest.UseKeyring(t)
	const msg = "custom fail message for stderr capture"
	vaulttest.Put(t, vault.Meta, "key_command", writeFailingKeyCommandScript(t, msg))

	_, _, err := vault.MasterKey()
	if err == nil {
		t.Fatal("MasterKey succeeded with a failing key_command")
	}
	if !strings.Contains(err.Error(), msg) {
		t.Errorf("MasterKey error = %q, want it to contain the command's stderr %q", err.Error(), msg)
	}
}
