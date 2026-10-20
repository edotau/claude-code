package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
)

// useZeroRootVault points the package at a fresh TempDir vault with HARNESS_VAULT_KEY fixed to 32
// zero bytes, mirroring vaulttest.Use (unusable here: vaulttest imports this package).
func useZeroRootVault(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

// TestHKDFKnownAnswerRFC5869Case1 pins the exact stdlib call shape Phase 4b wires into parseKey —
// hkdf.Key(sha256.New, secret, salt, info, L) — against RFC 5869's published SHA-256 test case 1,
// independent of this package's own derivation so a broken OKM never hides behind a package bug.
func TestHKDFKnownAnswerRFC5869Case1(t *testing.T) {
	ikm := bytes.Repeat([]byte{0x0b}, 22)
	salt, err := hex.DecodeString("000102030405060708090a0b0c")
	if err != nil {
		t.Fatal(err)
	}
	info, err := hex.DecodeString("f0f1f2f3f4f5f6f7f8f9")
	if err != nil {
		t.Fatal(err)
	}
	wantOKM, err := hex.DecodeString("3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865")
	if err != nil {
		t.Fatal(err)
	}
	okm, err := hkdf.Key(sha256.New, ikm, salt, string(info), 42)
	if err != nil {
		t.Fatalf("hkdf.Key: %v", err)
	}
	if !bytes.Equal(okm, wantOKM) {
		t.Fatalf("hkdf.Key OKM = %x, want RFC 5869 case 1 %x", okm, wantOKM)
	}
}

// TestInfoChangeProducesDifferentSubkey pins that the info label is load-bearing in the derivation
// formula itself: same root and salt, a different info string, different output bytes.
func TestInfoChangeProducesDifferentSubkey(t *testing.T) {
	root := bytes.Repeat([]byte{0x42}, 32)
	salt := []byte("some-vault-id")
	k1, err := hkdf.Key(sha256.New, root, salt, secretsInfo, 32)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := hkdf.Key(sha256.New, root, salt, secretsInfo+"-other", 32)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k2) {
		t.Fatal("different info strings produced identical subkeys")
	}
}

// buildManualKey constructs a Key the same way parseKey does (HKDF subkey, AES-GCM with a random
// nonce), but from caller-supplied root/salt/info/kid instead of going through parseKey — used to
// prove the production Open actually depends on the real derivation, not merely trust parseKey.
func buildManualKey(t *testing.T, root []byte, salt, info, kid string) *Key {
	t.Helper()
	sub, err := hkdf.Key(sha256.New, root, []byte(salt), info, 32)
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
	return &Key{Kid: kid, aead: aead, raw: base64.StdEncoding.EncodeToString(root)}
}

// buildRawRootKey builds a Key straight from root bytes with no HKDF step at all — the pre-Phase-4b
// construction — so TestRawRootNoLongerOpensRecord can show the root itself is no longer sufficient.
func buildRawRootKey(t *testing.T, root []byte, kid string) *Key {
	t.Helper()
	block, err := aes.NewCipher(root)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		t.Fatal(err)
	}
	return &Key{Kid: kid, aead: aead, raw: base64.StdEncoding.EncodeToString(root)}
}

func currentVaultID(t *testing.T) string {
	t.Helper()
	var id string
	if err := View(func(tx *Tx) error {
		id = vaultID(tx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestSameRootTwoVaultIDsDeriveDifferentKeys is the core Phase 4b regression: a record sealed in
// vault A does not open in vault B even though both share the exact same root, because the salt
// (vault_id) differs. Before Phase 4b, parseKey ignored vaultID and this record opened everywhere.
func TestSameRootTwoVaultIDsDeriveDifferentKeys(t *testing.T) {
	useZeroRootVault(t) // vault A, HARNESS_VAULT_KEY = 32 zero bytes
	kA, err := EnsureKey()
	if err != nil {
		t.Fatalf("vault A EnsureKey: %v", err)
	}
	sealed := kA.Seal("NAME", "hello")
	record := Record{Sealed: sealed, Kid: kA.Kid}

	useZeroRootVault(t) // vault B: fresh TempDir, same root, different vault_id
	kB, err := EnsureKey()
	if err != nil {
		t.Fatalf("vault B EnsureKey: %v", err)
	}
	if kA.Kid != kB.Kid {
		t.Fatalf("same root produced different Kid: A=%q B=%q (Kid must stay root-only)", kA.Kid, kB.Kid)
	}
	if _, err := kB.Open("NAME", record); err == nil {
		t.Fatal("vault B opened a record sealed in vault A under the same root; vault_id salt is not wired")
	}
}

// TestRawRootNoLongerOpensRecord proves the root alone cannot decrypt a sealed record anymore: only
// the HKDF-derived subkey can. A pre-Phase-4b Key built straight from the root succeeded here.
func TestRawRootNoLongerOpensRecord(t *testing.T) {
	useZeroRootVault(t)
	k, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	root, err := base64.StdEncoding.DecodeString(k.raw)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Sealed: k.Seal("NAME", "value"), Kid: k.Kid}

	rawKey := buildRawRootKey(t, root, k.Kid)
	if _, err := rawKey.Open("NAME", record); err == nil {
		t.Fatal("a Key built directly from the raw root opened a record; HKDF subkey is not wired")
	}
}

// TestWrongInfoCannotOpen seals with a manually derived subkey using the wrong info label and shows
// the production Open (real root, real salt, real info) cannot decrypt it, and vice versa.
func TestWrongInfoCannotOpen(t *testing.T) {
	useZeroRootVault(t)
	k, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	id := currentVaultID(t)
	root, err := base64.StdEncoding.DecodeString(k.raw)
	if err != nil {
		t.Fatal(err)
	}

	wrongInfoKey := buildManualKey(t, root, id, secretsInfo+"-wrong", k.Kid)
	badRecord := Record{Sealed: wrongInfoKey.Seal("NAME", "value"), Kid: k.Kid}
	if _, err := k.Open("NAME", badRecord); err == nil {
		t.Fatal("production key opened a record sealed under the wrong info label")
	}

	goodRecord := Record{Sealed: k.Seal("NAME", "value"), Kid: k.Kid}
	if _, err := wrongInfoKey.Open("NAME", goodRecord); err == nil {
		t.Fatal("wrong-info key opened a record sealed by the production key")
	}
}

// TestSchemaWithSecretsRefused: an older-schema vault holding secrets is refused with the migrate
// message; the same older schema with zero secrets is accepted (and upgraded in place by Update).
func TestSchemaWithSecretsRefused(t *testing.T) {
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error {
		if err := tx.Put(Meta, "schema", "1"); err != nil {
			return err
		}
		return tx.PutRecord("NAME", Record{Sealed: []byte("ct"), Kid: "whatever"})
	}); err != nil {
		t.Fatal(err)
	}
	err := View(func(tx *Tx) error { return nil })
	if err == nil {
		t.Fatal("an older-schema vault holding secrets was accepted")
	}
	if !strings.Contains(err.Error(), "vault schema 1") || !strings.Contains(err.Error(), "vault migrate") {
		t.Fatalf("error = %q, want it to name the schema and the migrate verb", err.Error())
	}
}

func TestSchemaWithoutSecretsAcceptedAndUpgraded(t *testing.T) {
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error { return tx.Put(Meta, "schema", "1") }); err != nil {
		t.Fatal(err)
	}
	if err := View(func(tx *Tx) error { return nil }); err != nil {
		t.Fatalf("an older schema with zero secrets was refused: %v", err)
	}
	// A subsequent Update upgrades the stamped schema in place.
	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := View(func(tx *Tx) error {
		s, _ := tx.Get(Meta, "schema")
		if s != strconv.Itoa(schemaVersion) {
			t.Fatalf("schema after Update = %q, want %d", s, schemaVersion)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRotateKeyRederivesUnderNewRoot: after RotateKey, the live record still opens (re-sealed under
// the new root's own subkey), and an archival copy made before rotation still opens via the retired
// root's own derived subkey — not via the raw retired root, and not via the new root.
func TestRotateKeyRederivesUnderNewRoot(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "")
	useSpecKeyring(t, &specKeyring{values: make(map[string]string)})
	oldKey, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	preRotation := Record{Sealed: oldKey.Seal("TOK", "v1"), Kid: oldKey.Kid}
	if err := Update(func(tx *Tx) error { return tx.PutRecord("TOK", preRotation) }); err != nil {
		t.Fatal(err)
	}

	oldKid, newKid, err := RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	if oldKid != oldKey.Kid {
		t.Fatalf("RotateKey oldKid = %q, want %q", oldKid, oldKey.Kid)
	}

	got, found, err := ReadSecret("TOK")
	if err != nil || !found || got != "v1" {
		t.Fatalf("ReadSecret after RotateKey = %q, %v, %v, want v1, true, nil", got, found, err)
	}

	id := currentVaultID(t)
	raw, found, err := keyringLookup("vault-key/" + id + "." + oldKid)
	if err != nil || !found {
		t.Fatalf("retired key lookup: found=%v err=%v", found, err)
	}
	retiredKey, err := parseKey(raw, id)
	if err != nil {
		t.Fatal(err)
	}
	if retiredKey.Kid != oldKid {
		t.Fatalf("retired key Kid = %q, want %q", retiredKey.Kid, oldKid)
	}
	plain, err := retiredKey.Open("TOK", preRotation)
	if err != nil || plain != "v1" {
		t.Fatalf("retired root's derived key could not open the pre-rotation copy: %q, %v", plain, err)
	}

	newRoot, err := base64.StdEncoding.DecodeString(retiredKey.raw)
	if err == nil {
		rawRetiredKey := buildRawRootKey(t, newRoot, oldKid)
		if _, err := rawRetiredKey.Open("TOK", preRotation); err == nil {
			t.Fatal("the raw retired root (no HKDF) opened the pre-rotation copy")
		}
	}
	_ = newKid
}
