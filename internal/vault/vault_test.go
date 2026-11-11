package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/schemas"
	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

// useZeroRootVault points the package at a fresh TempDir vault with HARNESS_VAULT_KEY fixed to 32
// zero bytes, mirroring vaulttest.Use (unusable here: vaulttest imports this package).
func useZeroRootVault(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("CLAUDE_PROFILE", "") // the launching session's marker must not steer a hermetic vault
	return dir
}

// useKeyringVault is a fresh vault whose master key resolves through an in-memory keyring, not the env var.
func useKeyringVault(t *testing.T) (string, *specKeyring) {
	t.Helper()
	dir := useZeroRootVault(t)
	t.Setenv("HARNESS_VAULT_KEY", "")
	ring := &specKeyring{values: make(map[string]string)}
	useSpecKeyring(t, ring)
	return dir, ring
}

// putVault writes one raw bucket value.
func putVault(t *testing.T, bucket, key, val string) {
	t.Helper()
	if err := Update(func(tx *Tx) error { return tx.Put(bucket, key, val) }); err != nil {
		t.Fatal(err)
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

// memory_origins rows go through the unsealed Put/Get/All path with no master key; an empty Put deletes the row.
func TestMemoryOriginsPlaintextRows(t *testing.T) {
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
		t.Fatalf("Update(memory_origins) without a master key: %v", err)
	}
	var got map[string]string
	if err := View(func(tx *Tx) error { got = tx.All(MemoryOrigins); return nil }); err != nil {
		t.Fatalf("View(memory_origins) without a master key: %v", err)
	}
	if len(got) != len(want) || got["alpha"] != want["alpha"] || got["beta"] != want["beta"] {
		t.Errorf("All(memory_origins) = %v, want %v", got, want)
	}
	if err := Update(func(tx *Tx) error { return tx.Put(MemoryOrigins, "beta", "") }); err != nil {
		t.Fatal(err)
	}
	if err := View(func(tx *Tx) error {
		if _, ok := tx.Get(MemoryOrigins, "beta"); ok {
			t.Error("row survived an empty-value Put")
		}
		if v, ok := tx.Get(MemoryOrigins, "alpha"); !ok || v != want["alpha"] {
			t.Errorf("Get(memory_origins, alpha) = %q, %v, want the stored row", v, ok)
		}
		if names := tx.Names(MemoryOrigins); len(names) != 1 || names[0] != "alpha" {
			t.Errorf("Names(memory_origins) = %v, want [alpha]", names)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
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

// TestNestedOpenErrors documents the architectural contract (View/Update are transaction
// boundaries; never call one from inside another's callback) with its actual, now-unguarded
// consequence: a same-goroutine nested open blocks on the outer transaction's own bbolt file
// lock until the INNER open's own Timeout elapses (2s for View/MasterKey, 5s for Update), then
// fails with bbolt.ErrTimeout. There is no process-wide or goroutine-keyed guard (deleted per the
// Phase 1/1b review: runtime.Stack parsing is not a supported synchronization API and missed a
// callback that hands off to another goroutine); the cost of a real violation is a bounded stall
// and a clear error, not a silent deadlock or data race.
func TestNestedOpenErrors(t *testing.T) {
	phase1bRing(t)
	const shortTimeout = 150 * time.Millisecond
	oldRead, oldWrite := readLockTimeout, writeLockTimeout
	readLockTimeout, writeLockTimeout = shortTimeout, shortTimeout
	t.Cleanup(func() { readLockTimeout, writeLockTimeout = oldRead, oldWrite })
	for _, operation := range []string{"View", "Update", "MasterKey"} {
		t.Run(operation, func(t *testing.T) {
			elapsed, nested := nestedOpen(t, operation)
			if !errors.Is(nested, bolterrors.ErrTimeout) || elapsed < shortTimeout/2 || elapsed >= 3*time.Second {
				t.Fatalf("nested %s = %v after %s, want bbolt.ErrTimeout bounded by its own Timeout", operation, nested, elapsed)
			}
		})
	}
}

// nestedOpen runs operation inside an Update callback and returns its error and how long it blocked.
func nestedOpen(t *testing.T, operation string) (elapsed time.Duration, nested error) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- Update(func(tx *Tx) error {
			start := time.Now()
			switch operation {
			case "View":
				nested = View(func(*Tx) error { return nil })
			case "Update":
				nested = Update(func(*Tx) error { return nil })
			case "MasterKey":
				_, _, nested = MasterKey()
			}
			elapsed = time.Since(start)
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nested open hung well beyond bbolt's own timeout")
	}
	return elapsed, nested
}

func TestVaultReaderHelper(t *testing.T) {
	if os.Getenv("PHASE1B_READER_HELPER") != "1" {
		return
	}
	ready := os.Getenv("PHASE1B_READER_READY")
	if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	stop := os.Getenv("PHASE1B_READER_STOP")
	deadline := time.Now().Add(10 * time.Second)
	reads := 0
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stop); err == nil {
			break
		}
		if err := View(func(tx *Tx) error {
			_, _ = tx.Get(Pins, "progress")
			return nil
		}); err != nil {
			t.Fatalf("reader failed after %d reads: %v", reads, err)
		}
		reads++
		time.Sleep(time.Millisecond)
	}
	if reads == 0 {
		t.Fatal("reader did not make progress")
	}
}

func TestMultiProcessProgress(t *testing.T) {
	phase1bRing(t)
	if err := Update(func(tx *Tx) error { return tx.Put(Pins, "progress", "initial") }); err != nil {
		t.Fatal(err)
	}
	stop := filepath.Join(t.TempDir(), "stop")
	children := startReaders(t, 4, stop)
	latencies := make([]time.Duration, 0, 30)
	for i := range 30 {
		start := time.Now()
		err := Update(func(tx *Tx) error { return tx.Put(Pins, "progress", strconv.Itoa(i)) })
		latencies = append(latencies, time.Since(start))
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(stop, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Errorf("reader subprocess: %v\n%s", err, c.output.String())
		}
	}
	slices.Sort(latencies)
	p95 := latencies[(95*len(latencies)+99)/100-1]
	t.Logf("30 writer latencies: %v; p95=%s", latencies, p95)
	if p95 >= 500*time.Millisecond {
		t.Fatalf("writer p95 %s exceeds spec limit 500ms", p95)
	}
}

// readerChild is one TestVaultReaderHelper subprocess and the file it creates once reading.
type readerChild struct {
	cmd    *exec.Cmd
	output strings.Builder
	ready  string
}

// startReaders starts n TestVaultReaderHelper subprocesses that read until stop exists, and waits until all are ready.
func startReaders(t *testing.T, n int, stop string) []*readerChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	children := make([]*readerChild, n)
	for i := range children {
		c := &readerChild{
			cmd:   exec.Command(executable, "-test.run=^TestVaultReaderHelper$", "-test.timeout=15s"),
			ready: filepath.Join(t.TempDir(), "ready"),
		}
		c.cmd.Env = append(os.Environ(), "PHASE1B_READER_HELPER=1", "PHASE1B_READER_READY="+c.ready, "PHASE1B_READER_STOP="+stop)
		c.cmd.Stdout, c.cmd.Stderr = &c.output, &c.output
		if err := c.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children[i] = c
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, c := range children {
		awaitFile(t, c.ready, deadline)
	}
	return children
}

// awaitFile polls until path exists, failing on any other stat error or once deadline passes.
func awaitFile(t *testing.T, path string, deadline time.Time) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("reader subprocess never became ready")
		}
		time.Sleep(time.Millisecond)
	}
}

// txOpeners are this package's own transaction-opening functions: a func literal passed as their
// argument is a callback that already runs inside an open transaction/sealed write.
var txOpeners = map[string]bool{"View": true, "ViewAt": true, "Update": true, "UpdateSealed": true}

// bannedInCallback are the calls such a callback must never make directly — each reopens a
// transaction (or MasterKey's own View) and stalls on the outer one's own bbolt flock until its
// Timeout elapses (TestNestedOpenErrors documents the runtime consequence; this is the static check
// the Phase 1/1b review asked for instead).
var bannedInCallback = map[string]bool{
	"View": true, "ViewAt": true, "Update": true, "UpdateSealed": true,
	"MasterKey": true, "EnsureKey": true, "ReadSecret": true, "RotateKey": true, "SnapshotTo": true,
}

// TestNoNestedVaultOpens statically walks every non-test .go file in this package for a func
// literal passed to View/ViewAt/Update/UpdateSealed and fails if that literal's own body calls a
// transaction-opening function directly (package-local, unqualified calls only — a transaction-local
// helper taking *Tx/*Key is not itself one of these names, so it is never flagged).
func TestNoNestedVaultOpens(t *testing.T) {
	for _, v := range findNestedOpens(t, ".") {
		t.Error(v)
	}
}

// findNestedOpens is split out so TestNoNestedVaultOpensCatchesIntroducedNesting can run it against
// a scratch copy with a deliberately injected nested call, proving the check has teeth.
func findNestedOpens(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				violations = append(violations, nestedInCall(fset, call)...)
			}
			return true
		})
	}
	return violations
}

// nestedInCall reports banned calls inside any func-literal argument of call when call opens a transaction.
func nestedInCall(fset *token.FileSet, call *ast.CallExpr) []string {
	ident, ok := call.Fun.(*ast.Ident)
	if !ok || !txOpeners[ident.Name] {
		return nil
	}
	var violations []string
	for _, arg := range call.Args {
		lit, ok := arg.(*ast.FuncLit)
		if !ok {
			continue
		}
		ast.Inspect(lit.Body, func(inner ast.Node) bool {
			innerCall, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if innerIdent, ok := innerCall.Fun.(*ast.Ident); ok && bannedInCallback[innerIdent.Name] {
				pos := fset.Position(innerCall.Pos())
				violations = append(violations, fmt.Sprintf(
					"%s:%d: a %s callback calls %s directly, nesting a vault transaction",
					pos.Filename, pos.Line, ident.Name, innerIdent.Name))
			}
			return true
		})
	}
	return violations
}

// TestNoNestedVaultOpensCatchesIntroducedNesting proves findNestedOpens has teeth: it injects a
// nested View() call into a scratch copy of an existing Update callback and confirms the check
// flags it, instead of trusting an untested detector.
func TestNoNestedVaultOpensCatchesIntroducedNesting(t *testing.T) {
	src, err := os.ReadFile("key.go")
	if err != nil {
		t.Fatal(err)
	}
	anchor := "id = vaultID(t)\n\t\treturn nil\n"
	if n := strings.Count(string(src), anchor); n != 1 {
		t.Fatalf("anchor appears %d times in key.go, want exactly 1 (source may have shifted)", n)
	}
	injected := strings.Replace(string(src), anchor,
		"id = vaultID(t)\n\t\tView(func(*Tx) error { return nil })\n\t\treturn nil\n", 1)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "key.go"), []byte(injected), 0600); err != nil {
		t.Fatal(err)
	}
	if violations := findNestedOpens(t, dir); len(violations) == 0 {
		t.Fatal("injected nested View() inside an Update callback was not detected — the guard has no teeth")
	}
}

func ledgerLines(t *testing.T) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(schemas.LedgerPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for l := range bytes.SplitSeq(bytes.TrimSpace(b), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// noopUpdate runs an empty Update, which still applies any pending re-stamp.
func noopUpdate(t *testing.T) {
	t.Helper()
	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// checkStoredSchema reports meta/schema with format unless it equals want.
func checkStoredSchema(t *testing.T, want, format string) {
	t.Helper()
	if err := View(func(tx *Tx) error {
		if s, _ := tx.Get(Meta, "schema"); s != want {
			t.Errorf(format, s)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestVaultSchema2RestampedOnFirstUpdateWithOneLedgerLine pins: schema 2 -> 3 on first Update with one ledger line; a fresh vault logs none.
func TestVaultSchema2RestampedOnFirstUpdateWithOneLedgerLine(t *testing.T) {
	useZeroRootVault(t)
	noopUpdate(t)
	if l := ledgerLines(t); len(l) != 0 {
		t.Fatalf("fresh vault wrote ledger lines: %v", l)
	}

	if err := Update(func(tx *Tx) error { return tx.SetSchema(2) }); err != nil {
		t.Fatal(err)
	}
	noopUpdate(t)
	checkStoredSchema(t, strconv.Itoa(schemaVersion), "schema after Update = %q, want current")
	l := ledgerLines(t)
	if len(l) != 1 || l[0]["format"] != "vault" || l[0]["from"] != float64(2) || l[0]["to"] != float64(schemaVersion) {
		t.Fatalf("ledger = %v, want one {format:vault from:2 to:current}", l)
	}

	noopUpdate(t)
	if n := len(ledgerLines(t)); n != 1 {
		t.Errorf("a second Update added ledger lines: %d total", n)
	}
}

// TestUpdateErrorLeavesNoLedgerLine pins: a failed Update rolls back the 2->3 stamp and logs nothing; a later success logs one line.
func TestUpdateErrorLeavesNoLedgerLine(t *testing.T) {
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error { return tx.SetSchema(2) }); err != nil {
		t.Fatal(err)
	}
	// SetSchema(2) is itself an Update at the current version; clear anything it logged.
	base := len(ledgerLines(t))

	boom := errors.New("boom")
	if err := Update(func(tx *Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Update err = %v, want boom", err)
	}
	checkStoredSchema(t, strconv.Itoa(2), "schema after failed Update = %q, want 2")
	if n := len(ledgerLines(t)); n != base {
		t.Fatalf("failed Update logged %d ledger line(s), want 0", n-base)
	}

	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	l := ledgerLines(t)
	if len(l) != base+1 || l[base]["from"] != float64(2) || l[base]["to"] != float64(schemaVersion) {
		t.Fatalf("ledger = %v, want exactly one new 2->current line", l)
	}
}

func TestSnapshotToConsistent(t *testing.T) {
	phase1bRing(t)
	const base = 64
	if err := Update(func(tx *Tx) error {
		for i := range base {
			if err := tx.PutRecord(fmt.Sprintf("TOKEN_%03d", i), Record{Prefix: fmt.Sprintf("value-%d", i), Len: i + 1}); err != nil {
				return err
			}
		}
		return tx.Put(Meta, "record_count", strconv.Itoa(base))
	}); err != nil {
		t.Fatal(err)
	}
	started, stop, done, writes := startRecordWriter(base)
	<-started
	snapshot := filepath.Join(t.TempDir(), "snapshot.db")
	f, err := os.OpenFile(snapshot, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		close(stop)
		<-done
		t.Fatal(err)
	}
	err = SnapshotTo(Path(), f)
	closeErr := f.Close()
	close(stop)
	writerErr := <-done
	if err != nil || closeErr != nil || writerErr != nil {
		t.Fatalf("snapshot=%v close=%v concurrent writer=%v", err, closeErr, writerErr)
	}
	if writes.Load() == 0 {
		t.Fatal("concurrent writer never committed; snapshot fixture was vacuous")
	}
	checkSnapshotGeneration(t, snapshot, base)
	checkSnapshotMissingSource(t)
}

// checkSnapshotMissingSource asserts SnapshotTo from an absent vault fails with fs.ErrNotExist.
func checkSnapshotMissingSource(t *testing.T) {
	t.Helper()
	missing, err := os.CreateTemp(t.TempDir(), "missing-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Close()
	if err := SnapshotTo(filepath.Join(t.TempDir(), "absent.db"), missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing snapshot source = %v, want fs.ErrNotExist", err)
	}
}

// startRecordWriter keeps committing TOKEN_n plus meta/record_count from base until stop; started closes after the first.
func startRecordWriter(base int) (started, stop chan struct{}, done chan error, writes *atomic.Int32) {
	started, stop, done, writes = make(chan struct{}), make(chan struct{}), make(chan error, 1), new(atomic.Int32)
	go func() {
		for n := base; ; n++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := Update(func(tx *Tx) error {
				if err := tx.PutRecord(fmt.Sprintf("TOKEN_%03d", n), Record{Prefix: fmt.Sprintf("value-%d", n), Len: n + 1}); err != nil {
					return err
				}
				return tx.Put(Meta, "record_count", strconv.Itoa(n+1))
			}); err != nil {
				if n == base {
					close(started)
				}
				done <- err
				return
			}
			writes.Add(1)
			if n == base {
				close(started)
			}
			// bbolt polls a blocked flock every 50ms; a gapless writer starves the snapshot's shared lock.
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return started, stop, done, writes
}

// checkSnapshotGeneration opens snapshot read-only and asserts it holds one committed generation of at least base.
func checkSnapshotGeneration(t *testing.T, snapshot string, base int) {
	t.Helper()
	db, err := bbolt.Open(snapshot, 0600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("snapshot does not open read-only: %v", err)
	}
	defer db.Close()
	if err := db.View(func(tx *bbolt.Tx) error { return snapshotGenerationErr(tx, base) }); err != nil {
		t.Fatal(err)
	}
}

// snapshotGenerationErr reports a snapshot whose record_count, record count or record contents disagree.
func snapshotGenerationErr(tx *bbolt.Tx, base int) error {
	records := tx.Bucket([]byte(Secrets))
	meta := tx.Bucket([]byte(Meta))
	if records == nil || meta == nil {
		return errors.New("snapshot missing records or metadata bucket")
	}
	count, err := strconv.Atoi(string(meta.Get([]byte("record_count"))))
	if err != nil || count < base || records.Stats().KeyN != count {
		return fmt.Errorf("snapshot is not a committed generation: count=%d records=%d err=%v",
			count, records.Stats().KeyN, err)
	}
	return snapshotRecordsErr(&Tx{tx}, count)
}

// snapshotRecordsErr reports the first of TOKEN_000..count-1 that is missing or holds the wrong prefix/len.
func snapshotRecordsErr(tx *Tx, count int) error {
	for i := range count {
		r, ok, rerr := tx.Record(fmt.Sprintf("TOKEN_%03d", i))
		if rerr != nil {
			return rerr
		}
		if !ok || r.Prefix != fmt.Sprintf("value-%d", i) || r.Len != i+1 {
			return fmt.Errorf("snapshot lost/corrupted record %d: %+v found=%t", i, r, ok)
		}
	}
	return nil
}
