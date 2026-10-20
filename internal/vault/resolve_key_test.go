package vault_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
