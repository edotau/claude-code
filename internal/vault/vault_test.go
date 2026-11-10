package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
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

// TestSchemaWithSecretsRefused: an older-schema vault holding secrets is refused with the honest
// "this build cannot read its secrets" message, not "vault migrate" advice this build cannot act
// on; the same older schema with zero secrets is accepted (and upgraded in place by Update).
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
	if !strings.Contains(err.Error(), "vault schema 1") || !strings.Contains(err.Error(), "cannot read its secrets") {
		t.Fatalf("error = %q, want it to name the schema and say it cannot read the secrets", err.Error())
	}
	if strings.Contains(err.Error(), "vault migrate") {
		t.Errorf("error = %q, must not advise a migrate this build cannot run", err.Error())
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

	oldKid, _, err := RotateKey()
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

	retiredKey := retiredKeyring(t, oldKid)
	plain, err := retiredKey.Open("TOK", preRotation)
	if err != nil || plain != "v1" {
		t.Fatalf("retired root's derived key could not open the pre-rotation copy: %q, %v", plain, err)
	}

}

// retiredKeyring parses the key kept under oldKid's immutable account and asserts it carries oldKid.
func retiredKeyring(t *testing.T, oldKid string) *Key {
	t.Helper()
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
	return retiredKey
}

// Names must list a bucket's keys in sorted (bbolt byte) order, excluding any nested bucket, and
// return an empty non-nil slice for a nil transaction.
func TestNamesOrderAndNoValues(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))

	const bucket = "namestest"
	if err := Update(func(tx *Tx) error {
		for _, k := range []string{"b", "a", "B", "a0"} {
			if err := tx.Put(bucket, k, "v"); err != nil {
				return err
			}
		}
		// A nested bucket inside the same top-level bucket must never show up as a "name".
		_, err := tx.tx.Bucket([]byte(bucket)).CreateBucket([]byte("nested"))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := View(func(tx *Tx) error {
		got = tx.Names(bucket)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"B", "a", "a0", "b"}
	if !slices.Equal(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}

	var nilTx Tx
	if got := nilTx.Names(bucket); got == nil || len(got) != 0 {
		t.Errorf("Names() on nil Tx = %#v, want empty non-nil slice", got)
	}
}

// TestMemoryOriginsBucketIsPlaintextAndNeedsNoMasterKey: the memory_origins bucket is read/written
// through the ordinary (unsealed) Put/Get path — a caller with no master key at all must succeed.
func TestMemoryOriginsBucketIsPlaintextAndNeedsNoMasterKey(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "")

	if err := Update(func(tx *Tx) error { return tx.Put(MemoryOrigins, "repo-a", `{"version":1}`) }); err != nil {
		t.Fatalf("Update(memory_origins) without a master key: %v", err)
	}
	var got string
	var ok bool
	if err := View(func(tx *Tx) error { got, ok = tx.Get(MemoryOrigins, "repo-a"); return nil }); err != nil {
		t.Fatalf("View(memory_origins) without a master key: %v", err)
	}
	if !ok || got != `{"version":1}` {
		t.Errorf("Get(memory_origins, repo-a) = %q, %v, want the stored row", got, ok)
	}
}

// TestMemoryOriginsPutEmptyDeletesRow: Put's generic empty-value-deletes rule applies to this bucket
// the same as any other — the row disappears from both Get and All/Names.
func TestMemoryOriginsPutEmptyDeletesRow(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "")

	if err := Update(func(tx *Tx) error { return tx.Put(MemoryOrigins, "repo-b", `{"version":1}`) }); err != nil {
		t.Fatal(err)
	}
	if err := Update(func(tx *Tx) error { return tx.Put(MemoryOrigins, "repo-b", "") }); err != nil {
		t.Fatal(err)
	}
	if err := View(func(tx *Tx) error {
		if _, ok := tx.Get(MemoryOrigins, "repo-b"); ok {
			t.Error("row survived an empty-value Put")
		}
		if names := tx.Names(MemoryOrigins); len(names) != 0 {
			t.Errorf("Names(memory_origins) = %v, want empty", names)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMemoryOriginsAllReadsEveryRowInOneTransaction: All returns every stored row in a single View,
// the batched read resolveOrigins relies on to resolve a whole store in one vault round trip.
func TestMemoryOriginsAllReadsEveryRowInOneTransaction(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "")

	want := map[string]string{"alpha": `{"key":"alpha"}`, "beta": `{"key":"beta"}`}
	if err := Update(func(tx *Tx) error {
		for k, v := range want {
			if err := tx.Put(MemoryOrigins, k, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := View(func(tx *Tx) error { got = tx.All(MemoryOrigins); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || got["alpha"] != want["alpha"] || got["beta"] != want["beta"] {
		t.Errorf("All(memory_origins) = %v, want %v", got, want)
	}
}

func phase1bRing(t *testing.T) *specKeyring {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", "")
	ring := &specKeyring{values: make(map[string]string)}
	useSpecKeyring(t, ring)
	return ring
}

func phase1bMeta(t *testing.T, name string) string {
	t.Helper()
	var value string
	if err := View(func(tx *Tx) error {
		value, _ = tx.Get(Meta, name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

func phase1bPut(t *testing.T, name, value string) {
	t.Helper()
	if err := UpdateSealed(func(tx *Tx, key *Key) error {
		return tx.PutRecord(name, Record{Sealed: key.Seal(name, value), Kid: key.Kid, Len: len(value)})
	}); err != nil {
		t.Fatal(err)
	}
}

func phase1bRead(t *testing.T, name, want string) {
	t.Helper()
	value, found, err := ReadSecret(name)
	if err != nil || !found || value != want {
		t.Fatalf("ReadSecret(%q) = (%q, %t, %v), want %q, true, nil", name, value, found, err, want)
	}
}

// TestMalformedRecordSurfacesAsError reproduces the review's MEDIUM #5 scenario directly: a
// stored value that fails to decode as JSON must be reported as an error from Record/Records and
// from ReadSecret, never collapsed into an indistinguishable "not found" (R5).
func TestMalformedRecordSurfacesAsError(t *testing.T) {
	phase1bRing(t)
	phase1bPut(t, "GOOD", "fine")
	if err := Update(func(tx *Tx) error { return tx.Put(Secrets, "CORRUPT", "not valid json") }); err != nil {
		t.Fatal(err)
	}
	if err := View(func(tx *Tx) error {
		if _, ok, err := tx.Record("CORRUPT"); err == nil || ok {
			t.Fatalf("Record(CORRUPT) = (ok=%t, err=%v), want a decode error", ok, err)
		}
		if _, err := tx.Records(); err == nil {
			t.Fatal("Records() silently omitted the corrupt entry instead of erroring")
		}
		// An absent key must still read as a clean miss, not an error.
		if _, ok, err := tx.Record("ABSENT"); ok || err != nil {
			t.Fatalf("Record(ABSENT) = (ok=%t, err=%v), want (false, nil)", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadSecret("CORRUPT"); err == nil {
		t.Fatal("ReadSecret(CORRUPT) returned no error for a malformed stored record")
	}
	phase1bRead(t, "GOOD", "fine") // corruption on one key must not affect reading another
}

func TestTwoRootsBootstrapDistinctAccounts(t *testing.T) {
	ring := phase1bRing(t)
	roots := []string{os.Getenv("CLAUDE_CONFIG_DIR"), t.TempDir()}
	ids := make([]string, len(roots))
	values := make([]string, len(roots))
	kids := make([]string, len(roots))
	for i, root := range roots {
		t.Setenv("CLAUDE_CONFIG_DIR", root)
		ids[i], kids[i], values[i] = bootstrapRoot(t, ring, i)
	}
	if ids[0] == ids[1] || values[0] == values[1] || kids[0] == kids[1] {
		t.Fatal("distinct roots share an identity or master key")
	}
	checkRootAccounts(t, ring, ids, kids, values)
	for i, root := range roots {
		t.Setenv("CLAUDE_CONFIG_DIR", root)
		phase1bRead(t, "TOKEN", fmt.Sprintf("root-%d", i))
	}
}

// bootstrapRoot writes root i's first secret and returns its vault_id, kid and the keyring pointer's value.
func bootstrapRoot(t *testing.T, ring *specKeyring, i int) (id, kid, value string) {
	t.Helper()
	phase1bPut(t, "TOKEN", fmt.Sprintf("root-%d", i))
	id = phase1bMeta(t, "vault_id")
	rawID, err := hex.DecodeString(id)
	if err != nil || len(rawID) != 16 {
		t.Fatalf("vault_id %q is not 16 random bytes encoded as hex", id)
	}
	kid = phase1bMeta(t, "kid")
	ring.mu.Lock()
	value = ring.values["vault-key/"+id]
	ring.mu.Unlock()
	if value == "" {
		t.Fatalf("root %d has no per-vault keyring account", i)
	}
	return id, kid, value
}

// checkRootAccounts asserts each root kept its own pointer and per-kid account, and nothing else is stored.
func checkRootAccounts(t *testing.T, ring *specKeyring, ids, kids, values []string) {
	t.Helper()
	ring.mu.Lock()
	defer ring.mu.Unlock()
	for i, id := range ids {
		if ring.values["vault-key/"+id] != values[i] {
			t.Errorf("bootstrap overwrote root %d's master key", i)
		}
	}
	// Each root's genesis key lands under its convenience pointer AND its immutable per-kid
	// account (R1): two roots, two accounts each.
	if len(ring.values) != 4 {
		t.Errorf("keyring has %d accounts, want exactly the two roots' pointer + per-kid accounts", len(ring.values))
	}
	for i, id := range ids {
		if ring.values["vault-key/"+id+"."+kids[i]] != values[i] {
			t.Errorf("root %d's genesis key missing from its immutable per-kid account", i)
		}
	}
}

func TestEnsureKeyUnavailableDoesNotGenerate(t *testing.T) {
	for _, cause := range []error{ErrKeyringUnavailable, ErrKeyringNoAnswer} {
		t.Run(cause.Error(), func(t *testing.T) {
			ring := phase1bRing(t)
			ring.err = cause
			key, err := EnsureKey()
			if key != nil || !errors.Is(err, ErrLocked) {
				t.Fatalf("EnsureKey = (%v, %v), want nil, ErrLocked for %v", key, err, cause)
			}
			if !strings.Contains(err.Error(), "HARNESS_VAULT_KEY") || !strings.Contains(err.Error(), "key_command") {
				t.Errorf("locked error omits recovery instructions: %v", err)
			}
			checkRingUntouched(t, ring)
			if kid := phase1bMeta(t, "kid"); kid != "" {
				t.Fatalf("unavailable keyring persisted kid %q", kid)
			}
		})
	}
}

// checkRingUntouched asserts ring was consulted but never written or populated.
func checkRingUntouched(t *testing.T, ring *specKeyring) {
	t.Helper()
	ring.mu.Lock()
	sets, entries, lookups := ring.sets, len(ring.values), len(ring.lookups)
	ring.mu.Unlock()
	if sets != 0 || entries != 0 || lookups == 0 {
		t.Fatalf("unavailable keyring mutated: sets=%d entries=%d lookups=%d", sets, entries, lookups)
	}
}
