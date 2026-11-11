package vault

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMasterKeyRecoversPerKidAfterCommandFailure(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "recoverable-value")
	wantKid := phase1bMeta(t, "kid")
	acct := account(currentVaultID(t))
	ring.mu.Lock()
	delete(ring.values, acct)
	ring.mu.Unlock()
	if err := Update(func(tx *Tx) error {
		return tx.Put(Meta, "key_command", `["/bin/true"]`)
	}); err != nil {
		t.Fatal(err)
	}
	invalidateKey()
	key, source, err := MasterKey()
	if err != nil {
		t.Fatalf("MasterKey with missing pointer and empty command output: %v", err)
	}
	if key == nil || source != "keyring" || key.Kid != wantKid {
		t.Fatalf("MasterKey = %v, %q; want per-kid key %q from keyring", key, source, wantKid)
	}
	checkPointerThenPerKid(t, ring, acct, wantKid)
	phase1bRead(t, "TOKEN", "recoverable-value")
}

func TestRotateKeyForceAcceptsCurrentEnvKey(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "survives-forced-rotation")
	acct := account(currentVaultID(t))
	t.Setenv("HARNESS_VAULT_KEY", ringValue(ring, acct))
	before := phase1bMeta(t, "kid")
	if _, _, err := RotateKey(); err == nil {
		t.Fatal("unforced rotation accepted an env-held key")
	}
	oldKid, newKid, err := RotateKey(ForceRotate())
	if err != nil {
		t.Fatalf("forced rotation with current key in env: %v", err)
	}
	if oldKid != before || newKid == "" || newKid == before {
		t.Fatalf("rotation kids = %q -> %q, want %q -> a new kid", oldKid, newKid, before)
	}
	t.Setenv("HARNESS_VAULT_KEY", ringValue(ring, acct+"."+newKid))
	checkRecordKid(t, "TOKEN", newKid)
	phase1bRead(t, "TOKEN", "survives-forced-rotation")
}

// TestMasterKeyResolvesViaPerKidAccountOnPointerMismatch covers R1/R2's replacement for the
// retired ".next" pending-key scheme: when the convenience current-key pointer disagrees with the
// db's recorded kid (a crashed or cross-root rotation), MasterKey must still resolve the exact key
// via its own immutable per-kid account — durable storage, not a disposable pending slot.
func TestMasterKeyResolvesViaPerKidAccountOnPointerMismatch(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	oldBytes := bytes.Repeat([]byte{0x31}, 32)
	newBytes := bytes.Repeat([]byte{0x72}, 32)
	newHash := sha256.Sum256(newBytes)
	newKid := hex.EncodeToString(newHash[:])[:16]
	ring := &specKeyring{values: make(map[string]string)}
	useSpecKeyring(t, ring)

	// Materialize the vault (and its real vault_id) before deriving the fixture key: the HKDF
	// subkey is salted by vault_id, so deriving one against a not-yet-created vault would mismatch.
	if err := Update(func(tx *Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	nextKey := envFixtureKey(t, newBytes, newKid)
	record := Record{Sealed: nextKey.Seal("ROTATED_TOKEN", "rotation-resume-value"), Kid: newKid}
	// Crash after the re-seal transaction commits, before the convenience pointer is advanced.
	if err := Update(func(tx *Tx) error {
		if err := tx.Put(Meta, "kid", newKid); err != nil {
			return err
		}
		return tx.PutRecord("ROTATED_TOKEN", record)
	}); err != nil {
		t.Fatal(err)
	}
	acct := stalePointerRing(t, ring, oldBytes, newBytes, newKid)

	key, source, err := MasterKey()
	if err != nil {
		t.Fatalf("MasterKey did not resolve the new kid via its immutable account: %v", err)
	}
	if key == nil {
		t.Fatal("MasterKey returned nil without an error")
	}
	if source != "keyring" || key.Kid != newKid {
		t.Errorf("resumed key: source=%q kid=%q, want keyring and %q", source, key.Kid, newKid)
	}
	got, err := key.Open("ROTATED_TOKEN", record)
	if err != nil || got != "rotation-resume-value" {
		t.Errorf("resumed key cannot decrypt committed record: got=%q err=%v", got, err)
	}
	checkPointerThenPerKid(t, ring, acct, newKid)
}

// stalePointerRing points the current-key account at oldRoot, stores newRoot under newKid, and clears the env key.
func stalePointerRing(t *testing.T, ring *specKeyring, oldRoot, newRoot []byte, newKid string) string {
	t.Helper()
	id := currentVaultID(t)
	if id == "" {
		t.Fatal("new vault has no vault_id")
	}
	acct := "vault-key/" + id
	ring.values[acct] = base64.StdEncoding.EncodeToString(oldRoot)            // stale pointer
	ring.values[acct+"."+newKid] = base64.StdEncoding.EncodeToString(newRoot) // durable, per-kid
	t.Setenv("HARNESS_VAULT_KEY", "")
	return acct
}

// envFixtureKey resolves root through HARNESS_VAULT_KEY and asserts it came from env with kid.
func envFixtureKey(t *testing.T, root []byte, kid string) *Key {
	t.Helper()
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(root))
	k, source, err := MasterKey()
	if err != nil {
		t.Fatalf("load next key from env for crash fixture: %v", err)
	}
	if k == nil {
		t.Fatal("MasterKey returned nil for the fixture key")
	}
	if source != "env" || k.Kid != kid {
		t.Fatalf("fixture key: source=%q kid=%q, want env and %q", source, k.Kid, kid)
	}
	return k
}

// checkPointerThenPerKid asserts ring saw lookups of both the current-key pointer and kid's per-kid account.
func checkPointerThenPerKid(t *testing.T, ring *specKeyring, acct, kid string) {
	t.Helper()
	ring.mu.Lock()
	defer ring.mu.Unlock()
	seenCurrent, seenPerKid := false, false
	for _, name := range ring.lookups {
		seenCurrent = seenCurrent || name == acct
		seenPerKid = seenPerKid || name == acct+"."+kid
	}
	if !seenCurrent || !seenPerKid {
		t.Errorf("MasterKey must try the pointer then the per-kid account on kid mismatch; lookups=%v", ring.lookups)
	}
}

// keyCommandHelperArgv builds a key_command argv that re-execs this test binary as
// TestKeyCommandHelperProcess instead of a POSIX shell script, so key_command tests run
// unmodified on every OS (review's LOW #3).
func keyCommandHelperArgv(extra ...string) []string {
	executable, err := os.Executable()
	if err != nil {
		panic(err)
	}
	argv := []string{executable, "-test.run=^TestKeyCommandHelperProcess$", "-test.timeout=10s"}
	return append(argv, extra...)
}

// TestKeyCommandHelperProcess is not a real test: run as a key_command subprocess (guarded by
// VAULT_KEY_COMMAND_HELPER), it prints the configured key value and, if VAULT_KEY_COMMAND_ARGS_FILE
// is set, records its own non-test-flag arguments for the caller to inspect.
func TestKeyCommandHelperProcess(t *testing.T) {
	if os.Getenv("VAULT_KEY_COMMAND_HELPER") != "1" {
		return
	}
	var extra []string
	for _, a := range os.Args[1:] {
		if !strings.HasPrefix(a, "-test.") {
			extra = append(extra, a)
		}
	}
	if argsFile := os.Getenv("VAULT_KEY_COMMAND_ARGS_FILE"); argsFile != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "%d\n", len(extra))
		for _, a := range extra {
			fmt.Fprintf(&b, "%s\n", a)
		}
		if err := os.WriteFile(argsFile, []byte(b.String()), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Fprintln(os.Stdout, os.Getenv("VAULT_KEY_COMMAND_VALUE"))
	os.Exit(0)
}

// TestResolveKeyTriesCommandAfterUnavailableKeyring reproduces the review's HIGH #3 scenario: an
// explicitly configured key_command must still unlock the vault when the keyring could not
// positively establish absence (ErrUnavailable — no tool, locked session, or Windows), not only
// on a clean miss.
func TestResolveKeyTriesCommandAfterUnavailableKeyring(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "value")
	key, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("VAULT_KEY_COMMAND_HELPER", "1")
	t.Setenv("VAULT_KEY_COMMAND_VALUE", key.raw)
	argv, err := json.Marshal(keyCommandHelperArgv())
	if err != nil {
		t.Fatal(err)
	}
	if err := Update(func(tx *Tx) error { return tx.Put(Meta, "key_command", string(argv)) }); err != nil {
		t.Fatal(err)
	}
	invalidateKey()
	ring.mu.Lock()
	ring.err = ErrKeyringUnavailable
	ring.mu.Unlock()

	got, source, err := MasterKey()
	if err != nil || got == nil || source != "command" {
		t.Fatalf("MasterKey = (%v, %q, %v), want the configured command to unlock an unavailable keyring", got, source, err)
	}
	if got.Kid != key.Kid {
		t.Fatalf("command-resolved kid %q, want %q", got.Kid, key.Kid)
	}
}

// TestResolveKeyUnavailableNoCommandIsLockedWithActionableMessage is the other half of #3: with no
// key_command configured, an unavailable keyring must still surface as ErrLocked with the recovery
// instructions, not the raw keyring error.
func TestResolveKeyUnavailableNoCommandIsLockedWithActionableMessage(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "value")
	invalidateKey()
	ring.mu.Lock()
	ring.err = ErrKeyringUnavailable
	ring.mu.Unlock()

	_, _, err := MasterKey()
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("MasterKey = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), "HARNESS_VAULT_KEY") || !strings.Contains(err.Error(), "key_command") {
		t.Errorf("locked error omits recovery instructions: %v", err)
	}
}

// TestKeyCommandArgvJSON stores meta/key_command as a validated JSON argv array (R6): no shell is
// ever involved, so an argument containing shell metacharacters, quotes, and a Windows-style
// backslash path all run as exactly the arguments given — never re-tokenized.
func TestKeyCommandArgvJSON(t *testing.T) {
	phase1bRing(t)
	// Kid identifies the root alone (independent of vaultID); this test only compares Kid.
	key, raw, err := randomKey("")
	if err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("VAULT_KEY_COMMAND_HELPER", "1")
	t.Setenv("VAULT_KEY_COMMAND_VALUE", raw)
	t.Setenv("VAULT_KEY_COMMAND_ARGS_FILE", argsFile)
	extra := []string{"literal;value", `has "quotes"`, `C:\Windows\style\path.exe`}
	argv := keyCommandHelperArgv(extra...)
	encoded, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	if err := Update(func(tx *Tx) error {
		return tx.Put(Meta, "key_command", string(encoded))
	}); err != nil {
		t.Fatal(err)
	}
	got, source, err := MasterKey()
	if err != nil || got == nil || source != "command" {
		t.Fatalf("MasterKey argv command = (%v, %q, %v)", got, source, err)
	}
	if got.Kid != key.Kid {
		t.Fatalf("key command returned kid %q, want %q", got.Kid, key.Kid)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d\n%s\n%s\n%s\n", len(extra), extra[0], extra[1], extra[2])
	if string(args) != want {
		t.Fatalf("argv capture %q, want %q (each argument literal, no shell re-tokenizing)", args, want)
	}
}

// parseKeyCommandArgv accepts only a JSON argv array with a nonempty executable; later arguments may be empty (R6).
func TestParseKeyCommandArgv(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      []string
	}{
		{"not_json", "helper --label vault", nil},
		{"json_string_not_array", `"helper"`, nil},
		{"empty_array", `[]`, nil},
		{"empty_executable", `["", "x"]`, nil},
		{"empty_later_argument", `["helper", "--label", ""]`, []string{"helper", "--label", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseKeyCommandArgv(tc.raw)
			if (err == nil) != (tc.want != nil) || !slices.Equal(got, tc.want) {
				t.Fatalf("parseKeyCommandArgv(%q) = %#v, %v; want %#v", tc.raw, got, err, tc.want)
			}
		})
	}
}

// TestResolveKeyFallsBackToPerKidAccount reproduces the review's MEDIUM #1 scenario: with a
// committed kid and an intact per-kid account, a missing or malformed convenience pointer must
// still resolve the key by consulting that per-kid account directly, never ErrLocked.
func TestResolveKeyFallsBackToPerKidAccount(t *testing.T) {
	for _, tc := range []string{"pointer_deleted", "pointer_garbage"} {
		t.Run(tc, func(t *testing.T) {
			ring := phase1bRing(t)
			phase1bPut(t, "TOKEN", "value")
			acct := "vault-key/" + phase1bMeta(t, "vault_id")
			invalidateKey()
			ring.mu.Lock()
			if tc == "pointer_deleted" {
				delete(ring.values, acct)
			} else {
				ring.values[acct] = "not-a-valid-key"
			}
			ring.mu.Unlock()
			phase1bRead(t, "TOKEN", "value")
		})
	}
}

// TestResolveKeyMissingEverythingIsLockedNeverBootstraps is the other half of MEDIUM #1: a kid
// recorded with neither a per-kid account nor a pointer must surface ErrLocked, never a freshly
// generated key.
func TestResolveKeyMissingEverythingIsLockedNeverBootstraps(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "value")
	acct := "vault-key/" + phase1bMeta(t, "vault_id")
	kid := phase1bMeta(t, "kid")
	invalidateKey()
	ring.mu.Lock()
	delete(ring.values, acct)
	delete(ring.values, acct+"."+kid)
	ring.mu.Unlock()
	key, err := EnsureKey()
	if key != nil || !errors.Is(err, ErrLocked) {
		t.Fatalf("EnsureKey = (%v, %v), want nil, ErrLocked", key, err)
	}
}

// ringValue reads one ring account under its lock.
func ringValue(ring *specKeyring, name string) string {
	ring.mu.Lock()
	defer ring.mu.Unlock()
	return ring.values[name]
}

func TestStaleWriterRetries(t *testing.T) {
	phase1bRing(t)
	phase1bPut(t, "BASE", "before")
	oldKid := phase1bMeta(t, "kid")
	selected, resume, selections := pauseFirstKeySelection(t)
	done, callbacks := startRaceWriter(t, oldKid)
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	select {
	case <-selected:
	case err := <-done:
		t.Fatalf("writer returned before pausing after key selection: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer never reached after-key seam")
	}
	newKid := rotateWhileWriterPaused(t, oldKid, release, done)
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("writer did not refresh and retry: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stale writer hung instead of retrying")
	}
	if callbacks.Load() != 1 {
		t.Fatalf("sealed callback ran %d times, want once after stale tx abort", callbacks.Load())
	}
	if selections.Load() != 2 {
		t.Fatalf("writer selected a key %d times, want stale selection then one refreshed retry", selections.Load())
	}
	checkRecordKid(t, "RACE", newKid)
	phase1bRead(t, "RACE", "after")
}

// pauseFirstKeySelection makes the first UpdateSealed key selection block on resume, counting every selection.
func pauseFirstKeySelection(t *testing.T) (selected, resume chan struct{}, selections *atomic.Int32) {
	selected, resume, selections = make(chan struct{}), make(chan struct{}), new(atomic.Int32)
	var once sync.Once
	saved := testHookAfterKey
	testHookAfterKey = func() {
		selections.Add(1)
		once.Do(func() {
			close(selected)
			<-resume
		})
	}
	t.Cleanup(func() { testHookAfterKey = saved })
	return selected, resume, selections
}

// startRaceWriter runs an UpdateSealed writing RACE that must never see a retired or stale kid.
func startRaceWriter(t *testing.T, oldKid string) (<-chan error, *atomic.Int32) {
	done := make(chan error, 1)
	callbacks := new(atomic.Int32)
	go func() {
		done <- UpdateSealed(func(tx *Tx, key *Key) error {
			callbacks.Add(1)
			kid, _ := tx.Get(Meta, "kid")
			if key.Kid == oldKid || key.Kid != kid {
				t.Errorf("callback ran with retired/stale kid %q; transaction kid %q", key.Kid, kid)
			}
			return tx.PutRecord("RACE", Record{Sealed: key.Seal("RACE", "after"), Kid: key.Kid})
		})
	}()
	return done, callbacks
}

// rotateWhileWriterPaused rotates past oldKid; on failure it releases and drains the writer before failing.
func rotateWhileWriterPaused(t *testing.T, oldKid string, release func(), done <-chan error) string {
	t.Helper()
	_, newKid, err := RotateKey()
	if err != nil {
		release()
		<-done
		t.Fatal(err)
	}
	if newKid == oldKid {
		release()
		<-done
		t.Fatal("rotation did not change kid")
	}
	return newKid
}

// checkRecordKid asserts name is stored and sealed under kid.
func checkRecordKid(t *testing.T, name, kid string) {
	t.Helper()
	if err := View(func(tx *Tx) error {
		record, ok, err := tx.Record(name)
		if err != nil {
			return err
		}
		if !ok || record.Kid != kid {
			t.Fatalf("writer committed under retired kid: record=%+v want kid %q", record, kid)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReadSecretRereadsAfterExternalRotation(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "before-external-rotation")
	vaultID := phase1bMeta(t, "vault_id")
	acct := "vault-key/" + vaultID
	oldKid := phase1bMeta(t, "kid")
	next, nextValue, err := randomKey(vaultID)
	if err != nil {
		t.Fatal(err)
	}
	// A new process has no key cache; force this reader through its keyring boundary.
	invalidateKey()
	lookup := keyringLookup
	var switched bool
	keyringLookup = func(name string) (string, bool, error) {
		if name == acct && !switched {
			switched = true
			if err := Update(func(tx *Tx) error {
				if err := tx.PutRecord("TOKEN", Record{
					Sealed: next.Seal("TOKEN", "after-external-rotation"), Kid: next.Kid,
				}); err != nil {
					return err
				}
				return tx.Put(Meta, "kid", next.Kid)
			}); err != nil {
				return "", false, fmt.Errorf("external rotation fixture: %w", err)
			}
			ring.mu.Lock()
			ring.values[acct+"."+oldKid] = ring.values[acct]
			ring.values[acct] = nextValue
			ring.mu.Unlock()
			// Inject the wrong-key observation at the boundary where the external rotation became visible.
			return "", false, ErrWrongKey
		}
		return lookup(name)
	}
	t.Cleanup(func() { keyringLookup = lookup })
	phase1bRead(t, "TOKEN", "after-external-rotation")
	if !switched {
		t.Fatal("read never crossed the external-rotation boundary")
	}
}

func TestRotateRetainsOldKey(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "archive-value")
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	oldKid := phase1bMeta(t, "kid")
	acct := "vault-key/" + phase1bMeta(t, "vault_id")
	archiveRoot, archive := archiveVault(t)
	oldValue := ringValue(ring, acct)
	if _, _, err := RotateKey(); err != nil {
		t.Fatal(err)
	}
	if retained := ringValue(ring, acct+"."+oldKid); retained != oldValue || retained == "" {
		t.Fatal("rotation did not retain the old key under its immutable kid account")
	}
	if err := ViewAt(archive, func(tx *Tx) error {
		record, ok, err := tx.Record("TOKEN")
		if err != nil {
			return err
		}
		if !ok || record.Kid != oldKid {
			t.Fatal("archive fixture did not retain its original record and kid")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", archiveRoot)
	phase1bRead(t, "TOKEN", "archive-value")
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	if _, _, err := RotateKey(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", archiveRoot)
	phase1bRead(t, "TOKEN", "archive-value")
	if ringValue(ring, acct+"."+oldKid) != oldValue {
		t.Fatal("later rotation overwrote the retained archive key")
	}
}

// archiveVault copies the live vault file into a fresh config root and returns that root and the copy's path.
func archiveVault(t *testing.T) (archiveRoot, archive string) {
	t.Helper()
	archiveRoot = t.TempDir()
	archive = filepath.Join(archiveRoot, "state", "vault.db")
	if err := os.MkdirAll(filepath.Dir(archive), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	return archiveRoot, archive
}

func TestConcurrentRotateSerialized(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "serial-rotations")
	vaultID := phase1bMeta(t, "vault_id")
	acct := "vault-key/" + vaultID
	oldKid := phase1bMeta(t, "kid")
	results := rotateTwiceConcurrently(t)
	if results[0].next == results[1].next {
		t.Fatal("concurrent rotations reused the same generated key")
	}
	if results[0].old != oldKid && results[1].old != oldKid {
		t.Fatal("neither rotation began with the original key")
	}
	if results[0].old != results[1].next && results[1].old != results[0].next {
		t.Fatalf("rotations did not form a serial chain: %+v", results)
	}
	finalKid := phase1bMeta(t, "kid")
	currentValue := checkRotationsRetained(t, ring, acct, results)
	current, err := parseKey(currentValue, vaultID)
	if err != nil || current.Kid != finalKid {
		t.Fatalf("current key/meta mismatch: parse=%v meta=%q", err, finalKid)
	}
	phase1bRead(t, "TOKEN", "serial-rotations")
}

// rotateResult is one RotateKey call's outcome.
type rotateResult struct {
	old, next string
	err       error
}

// rotateTwiceConcurrently starts two RotateKey calls at once and returns both results, failing on any error.
func rotateTwiceConcurrently(t *testing.T) []rotateResult {
	t.Helper()
	start := make(chan struct{})
	done := make(chan rotateResult, 2)
	for range 2 {
		go func() {
			<-start
			old, next, err := RotateKey()
			done <- rotateResult{old, next, err}
		}()
	}
	close(start)
	results := make([]rotateResult, 0, 2)
	for range 2 {
		select {
		case r := <-done:
			if r.err != nil {
				t.Fatalf("concurrent rotation: %v", r.err)
			}
			results = append(results, r)
		case <-time.After(15 * time.Second):
			t.Fatal("concurrent rotations did not complete")
		}
	}
	return results
}

// checkRotationsRetained asserts every old and new kid has its immutable account and returns the pointer's value.
func checkRotationsRetained(t *testing.T, ring *specKeyring, acct string, results []rotateResult) string {
	t.Helper()
	ring.mu.Lock()
	defer ring.mu.Unlock()
	for _, r := range results {
		if ring.values[acct+"."+r.old] == "" {
			t.Errorf("rotation did not retain kid %q", r.old)
		}
		if ring.values[acct+"."+r.next] == "" {
			t.Errorf("rotation did not durably store kid %q under its immutable account", r.next)
		}
	}
	return ring.values[acct]
}

func TestMasterKeyStatusRejectsCorruptSecrets(t *testing.T) {
	for _, malformed := range []bool{true, false} {
		t.Run(fmt.Sprintf("malformed-json=%t", malformed), func(t *testing.T) {
			phase1bRing(t)
			phase1bPut(t, "A_GOOD", "healthy-value")
			phase1bPut(t, "Z_BAD", "damaged-value")
			if count, source, err := MasterKeyStatus(); count != 2 || source != "keyring" || err != nil {
				t.Fatalf("healthy status = %d, %q, %v", count, source, err)
			}
			corruptHealthSecret(t, malformed)
			count, source, err := MasterKeyStatus()
			if count != 2 || source != "keyring" || err == nil {
				t.Fatalf("corrupt status = %d, %q, %v; want 2, keyring, error", count, source, err)
			}
		})
	}
}

func TestMasterKeyStatusEmptyAndUnreadable(t *testing.T) {
	phase1bRing(t)
	if count, source, err := MasterKeyStatus(); count != 0 || source != "not needed" || err != nil {
		t.Fatalf("empty vault: MasterKeyStatus = (%d, %q, %v), want (0, \"not needed\", nil)", count, source, err)
	}
	if err := Update(func(tx *Tx) error { return tx.Put(Meta, "schema", strconv.Itoa(schemaVersion+1)) }); err != nil {
		t.Fatal(err)
	}
	if count, source, err := MasterKeyStatus(); count != 0 || source != "" || err == nil {
		t.Fatalf("unreadable vault: MasterKeyStatus = (%d, %q, %v), want (0, \"\", err)", count, source, err)
	}
}

func corruptHealthSecret(t *testing.T, malformed bool) {
	t.Helper()
	if err := Update(func(tx *Tx) error {
		if malformed {
			return tx.Put(Secrets, "Z_BAD", "{")
		}
		r, _, err := tx.Record("Z_BAD")
		if err != nil {
			return err
		}
		r.Sealed[len(r.Sealed)-1] ^= 1
		return tx.PutRecord("Z_BAD", r)
	}); err != nil {
		t.Fatal(err)
	}
}
