package vault_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestRetiredMetadata(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "missing"},
		{name: "empty", raw: `[]`, want: []string{}},
		{name: "null", raw: `null`},
		{name: "valid", raw: `["old", "older"]`, want: []string{"old", "older"}},
		{name: "invalid JSON", raw: `[`, wantErr: true},
		{name: "wrong shape", raw: `{}`, wantErr: true},
		{name: "mixed types", raw: `["old", 42]`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.Use(t)
			if tc.raw != "" {
				vaulttest.Put(t, vault.Meta, "retired", tc.raw)
			}
			if err := vault.View(func(tx *vault.Tx) error {
				got, err := vault.Retired(tx)
				if (err != nil) != tc.wantErr {
					t.Fatalf("Retired error = %v, want error %t", err, tc.wantErr)
				}
				if tc.wantErr && !strings.Contains(err.Error(), "retired") {
					t.Errorf("Retired error lacks metadata context: %v", err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("Retired = %v, want %v", got, tc.want)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetiredAfterTwoRotations(t *testing.T) {
	vaulttest.UseKeyring(t)
	k, err := vault.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	old, next, err := vault.RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	if old != k.Kid {
		t.Fatalf("old = %s, want %s", old, k.Kid)
	}
	second, _, err := vault.RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	if second != next {
		t.Fatalf("second old = %s, want %s", second, next)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		got, err := vault.Retired(tx)
		if err != nil {
			return err
		}
		if want := []string{k.Kid, next}; !reflect.DeepEqual(got, want) {
			t.Errorf("Retired = %v, want %v", got, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// mismatchedEnvForKeyring returns a HARNESS_VAULT_KEY value whose kid (sha256(root)[:16] hex,
// matching parseKey's scheme) differs from k's, so a RotateKey call still resolves the master key
// via the keyring fallback (source == "keyring") even though the env var is set — the scenario
// the --force guard actually has to catch.
func mismatchedEnvForKeyring(t *testing.T, k *vault.Key) string {
	t.Helper()
	for b := range byte(255) {
		raw := make([]byte, 32)
		for i := range raw {
			raw[i] = b
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:])[:16] != k.Kid {
			return base64.StdEncoding.EncodeToString(raw)
		}
	}
	t.Fatal("could not find a mismatched fixture key")
	return ""
}

// TestRotateKeyRefusesWithEnvSetWithoutForce: RotateKey must refuse, not silently rotate out from
// under HARNESS_VAULT_KEY, when the env var is set and --force (vault.ForceRotate) was not passed.
func TestRotateKeyRefusesWithEnvSetWithoutForce(t *testing.T) {
	vaulttest.UseKeyring(t)
	k, err := vault.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_VAULT_KEY", mismatchedEnvForKeyring(t, k))

	// Sanity: MasterKey still resolves via the keyring fallback despite the mismatched env var.
	_, source, err := vault.MasterKey()
	if err != nil || source != "keyring" {
		t.Fatalf("fixture setup: MasterKey = source %q err %v, want keyring/nil", source, err)
	}

	_, _, err = vault.RotateKey()
	if err == nil {
		t.Fatal("RotateKey succeeded with HARNESS_VAULT_KEY set and no --force")
	}
	if !strings.Contains(err.Error(), "HARNESS_VAULT_KEY") {
		t.Errorf("RotateKey error = %q, want it to name HARNESS_VAULT_KEY", err.Error())
	}
}

// TestRotateKeyRefusesWithKeyCommandSetWithoutForce mirrors the env case for meta/key_command.
func TestRotateKeyRefusesWithKeyCommandSetWithoutForce(t *testing.T) {
	vaulttest.UseKeyring(t)
	if _, err := vault.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Meta, "key_command", `["/bin/true"]`)

	_, _, err := vault.RotateKey()
	if err == nil {
		t.Fatal("RotateKey succeeded with meta/key_command set and no --force")
	}
	if !strings.Contains(err.Error(), "key_command") {
		t.Errorf("RotateKey error = %q, want it to name key_command", err.Error())
	}
}

// TestRotateKeyRefusesWhenRetiredUnparsable: an unparsable meta/retired must refuse the rotation
// outright rather than silently overwriting it with a fresh one-entry history — the whole point
// of retaining every retired kid is that an old archive/reader can still find its key.
func TestRotateKeyRefusesWhenRetiredUnparsable(t *testing.T) {
	vaulttest.UseKeyring(t)
	k, err := vault.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Meta, "retired", "not valid json")

	_, _, err = vault.RotateKey()
	if err == nil {
		t.Fatal("RotateKey succeeded with an unparsable meta/retired")
	}
	if !strings.Contains(err.Error(), "retired") {
		t.Errorf("RotateKey error = %q, want it to name meta/retired", err.Error())
	}

	if err := vault.View(func(tx *vault.Tx) error {
		raw, _ := tx.Get(vault.Meta, "retired")
		if raw != "not valid json" {
			t.Errorf("meta/retired = %q, want it left untouched by the refused rotation", raw)
		}
		kid, _ := tx.Get(vault.Meta, "kid")
		if kid != k.Kid {
			t.Errorf("meta/kid = %q, want it still %q (rotation must not have committed)", kid, k.Kid)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRotateKeyForceBypassesKeyCommandGuard: vault.ForceRotate lets rotation proceed, and the new
// key is the one subsequent reads resolve.
func TestRotateKeyForceBypassesKeyCommandGuard(t *testing.T) {
	vaulttest.UseKeyring(t)
	k, err := vault.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Meta, "key_command", `["/bin/true"]`)

	oldKid, newKid, err := vault.RotateKey(vault.ForceRotate())
	if err != nil {
		t.Fatalf("RotateKey(ForceRotate()) = %v, want success", err)
	}
	if oldKid != k.Kid {
		t.Errorf("oldKid = %q, want %q", oldKid, k.Kid)
	}
	if newKid == "" || newKid == oldKid {
		t.Errorf("newKid = %q, want a fresh non-empty kid", newKid)
	}
}
