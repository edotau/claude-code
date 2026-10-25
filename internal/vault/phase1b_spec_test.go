package vault

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/keyring"
	"go.etcd.io/bbolt"
)

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
		phase1bPut(t, "TOKEN", fmt.Sprintf("root-%d", i))
		ids[i] = phase1bMeta(t, "vault_id")
		rawID, err := hex.DecodeString(ids[i])
		if err != nil || len(rawID) != 16 {
			t.Fatalf("vault_id %q is not 16 random bytes encoded as hex", ids[i])
		}
		kids[i] = phase1bMeta(t, "kid")
		ring.mu.Lock()
		values[i] = ring.values["vault-key/"+ids[i]]
		ring.mu.Unlock()
		if values[i] == "" {
			t.Fatalf("root %d has no per-vault keyring account", i)
		}
	}
	if ids[0] == ids[1] || values[0] == values[1] || kids[0] == kids[1] {
		t.Fatal("distinct roots share an identity or master key")
	}
	ring.mu.Lock()
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
	ring.mu.Unlock()
	for i, root := range roots {
		t.Setenv("CLAUDE_CONFIG_DIR", root)
		phase1bRead(t, "TOKEN", fmt.Sprintf("root-%d", i))
	}
}

func TestEnsureKeyUnavailableDoesNotGenerate(t *testing.T) {
	for _, cause := range []error{keyring.ErrUnavailable, keyring.ErrNoAnswer} {
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
			ring.mu.Lock()
			sets, deletes, entries, lookups := ring.sets, ring.deletes, len(ring.values), len(ring.lookups)
			ring.mu.Unlock()
			if sets != 0 || deletes != 0 || entries != 0 || lookups == 0 {
				t.Fatalf("unavailable keyring mutated: sets=%d deletes=%d entries=%d lookups=%d",
					sets, deletes, entries, lookups)
			}
			if kid := phase1bMeta(t, "kid"); kid != "" {
				t.Fatalf("unavailable keyring persisted kid %q", kid)
			}
		})
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
	ring.err = keyring.ErrUnavailable
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
	ring.err = keyring.ErrUnavailable
	ring.mu.Unlock()

	_, _, err := MasterKey()
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("MasterKey = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), "HARNESS_VAULT_KEY") || !strings.Contains(err.Error(), "key_command") {
		t.Errorf("locked error omits recovery instructions: %v", err)
	}
}

// TestCrossRootSequentialRotationBothDecrypt reproduces the review's HIGH #1 scenario: two config
// roots share a vault_id (B started as a copy of A's db file). Rotating A then B sequentially must
// not strand either copy's active key — once the per-process cache is cleared (a fresh process),
// both roots must still decrypt their record.
func TestCrossRootSequentialRotationBothDecrypt(t *testing.T) {
	phase1bRing(t)
	rootA := os.Getenv("CLAUDE_CONFIG_DIR")
	phase1bPut(t, "TOKEN", "shared-secret")

	rootB := t.TempDir()
	data, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootB, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "state", "vault.db"), data, 0600); err != nil {
		t.Fatal(err)
	}

	_, k1, err := RotateKey() // A: K0 -> K1
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_CONFIG_DIR", rootB)
	invalidateKey()
	_, k2, err := RotateKey() // B (still showing K0): K0 -> K2, overwriting the shared pointer
	if err != nil {
		t.Fatal(err)
	}
	if k1 == k2 {
		t.Fatal("rotations produced the same kid")
	}

	invalidateKey() // simulate a fresh process: no cached key for either root
	t.Setenv("CLAUDE_CONFIG_DIR", rootB)
	phase1bRead(t, "TOKEN", "shared-secret")

	invalidateKey()
	t.Setenv("CLAUDE_CONFIG_DIR", rootA)
	phase1bRead(t, "TOKEN", "shared-secret")
}

// TestRotateSurvivesCrashBeforePointerPromotion reproduces the review's HIGH #2 scenario directly:
// a rotation's reseal transaction commits (the new kid is current in the db, records resealed
// under it) but the process dies before the convenience current-key pointer is advanced.
// Immutable per-kid storage (R1) means this needs no separate "pending" bookkeeping to recover
// (R2): the stale pointer is irrelevant, because matchKey always finds a key under its own kid
// account regardless of what the pointer says.
func TestRotateSurvivesCrashBeforePointerPromotion(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "committed-before-promotion")
	old, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	vaultID := phase1bMeta(t, "vault_id")
	next, nextValue, err := randomKey(vaultID)
	if err != nil {
		t.Fatal(err)
	}
	acct := "vault-key/" + vaultID
	ring.mu.Lock()
	oldValue := ring.values[acct]
	// Simulate RotateKey's crash window: the new key is already durable under its own immutable
	// kid account, but the pointer (acct) was never advanced past the old key.
	ring.values[acct+"."+next.Kid] = nextValue
	ring.mu.Unlock()
	if err := Update(func(tx *Tx) error {
		if err := tx.PutRecord("TOKEN", Record{
			Sealed: next.Seal("TOKEN", "committed-before-promotion"), Kid: next.Kid,
		}); err != nil {
			return err
		}
		return tx.Put(Meta, "kid", next.Kid)
	}); err != nil {
		t.Fatal(err)
	}
	// Before any further rotation even runs, a plain read must already recover via the per-kid
	// account — no "promotion" step is required for K1 to survive.
	invalidateKey()
	phase1bRead(t, "TOKEN", "committed-before-promotion")

	_, gotKid, err := RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	ring.mu.Lock()
	current := ring.values[acct]
	retainedOld, retainedNext := ring.values[acct+"."+old.Kid], ring.values[acct+"."+next.Kid]
	ring.mu.Unlock()
	if current == "" || gotKid == "" || retainedOld != oldValue || retainedNext != nextValue {
		t.Fatalf("rotation after a crashed promotion lost a key: current=%t kid=%q retainedOld=%t retainedNext=%t",
			current != "", gotKid, retainedOld == oldValue, retainedNext == nextValue)
	}
	if kid := phase1bMeta(t, "kid"); kid != gotKid {
		t.Fatalf("recovery changed committed kid to %q, want %q", kid, gotKid)
	}
	phase1bRead(t, "TOKEN", "committed-before-promotion")
}

func TestStaleWriterRetries(t *testing.T) {
	phase1bRing(t)
	phase1bPut(t, "BASE", "before")
	oldKid := phase1bMeta(t, "kid")
	selected, resume := make(chan struct{}), make(chan struct{})
	var selections atomic.Int32
	var once sync.Once
	saved := testHookAfterKey
	testHookAfterKey = func() {
		selections.Add(1)
		once.Do(func() {
			close(selected)
			<-resume
		})
	}
	done := make(chan error, 1)
	var callbacks atomic.Int32
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
	t.Cleanup(func() { testHookAfterKey = saved })
	released := false
	defer func() {
		if !released {
			close(resume)
		}
	}()
	select {
	case <-selected:
	case err := <-done:
		t.Fatalf("writer returned before pausing after key selection: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer never reached after-key seam")
	}
	_, newKid, err := RotateKey()
	if err != nil {
		close(resume)
		released = true
		<-done
		t.Fatal(err)
	}
	if newKid == oldKid {
		close(resume)
		released = true
		<-done
		t.Fatal("rotation did not change kid")
	}
	close(resume)
	released = true
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
	if err := View(func(tx *Tx) error {
		record, ok, err := tx.Record("RACE")
		if err != nil {
			return err
		}
		if !ok || record.Kid != newKid {
			t.Fatalf("writer committed under retired kid: record=%+v want kid %q", record, newKid)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	phase1bRead(t, "RACE", "after")
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
			var nested error
			var elapsed time.Duration
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
			if !errors.Is(nested, bbolt.ErrTimeout) || elapsed < shortTimeout/2 || elapsed >= 3*time.Second {
				t.Fatalf("nested %s = %v after %s, want bbolt.ErrTimeout bounded by its own Timeout", operation, nested, elapsed)
			}
		})
	}
}

func TestRotateRetainsOldKey(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "archive-value")
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	oldKid := phase1bMeta(t, "kid")
	acct := "vault-key/" + phase1bMeta(t, "vault_id")
	archiveRoot := t.TempDir()
	archive := filepath.Join(archiveRoot, "state", "vault.db")
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
	ring.mu.Lock()
	oldValue := ring.values[acct]
	ring.mu.Unlock()
	if _, _, err := RotateKey(); err != nil {
		t.Fatal(err)
	}
	ring.mu.Lock()
	retained := ring.values[acct+"."+oldKid]
	ring.mu.Unlock()
	if retained != oldValue || retained == "" {
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
	ring.mu.Lock()
	defer ring.mu.Unlock()
	if ring.values[acct+"."+oldKid] != oldValue {
		t.Fatal("later rotation overwrote the retained archive key")
	}
}

func TestConcurrentRotateSerialized(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "serial-rotations")
	vaultID := phase1bMeta(t, "vault_id")
	acct := "vault-key/" + vaultID
	oldKid := phase1bMeta(t, "kid")
	type result struct {
		old, next string
		err       error
	}
	start := make(chan struct{})
	done := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			old, next, err := RotateKey()
			done <- result{old, next, err}
		}()
	}
	close(start)
	results := make([]result, 0, 2)
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
	ring.mu.Lock()
	currentValue := ring.values[acct]
	for _, r := range results {
		if ring.values[acct+"."+r.old] == "" {
			t.Errorf("rotation did not retain kid %q", r.old)
		}
		if ring.values[acct+"."+r.next] == "" {
			t.Errorf("rotation did not durably store kid %q under its immutable account", r.next)
		}
	}
	ring.mu.Unlock()
	current, err := parseKey(currentValue, vaultID)
	if err != nil || current.Kid != finalKid {
		t.Fatalf("current key/meta mismatch: parse=%v meta=%q", err, finalKid)
	}
	phase1bRead(t, "TOKEN", "serial-rotations")
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
	started, stop, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var writes atomic.Int32
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
	db, err := bbolt.Open(snapshot, 0600, &bbolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("snapshot does not open read-only: %v", err)
	}
	defer db.Close()
	if err := db.View(func(tx *bbolt.Tx) error {
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
		for i := range count {
			r, ok, rerr := (&Tx{tx}).Record(fmt.Sprintf("TOKEN_%03d", i))
			if rerr != nil {
				return rerr
			}
			if !ok || r.Prefix != fmt.Sprintf("value-%d", i) || r.Len != i+1 {
				return fmt.Errorf("snapshot lost/corrupted record %d: %+v found=%t", i, r, ok)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	missing, err := os.CreateTemp(t.TempDir(), "missing-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Close()
	if err := SnapshotTo(filepath.Join(t.TempDir(), "absent.db"), missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing snapshot source = %v, want fs.ErrNotExist", err)
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

// TestKeyCommandArgvRejectsBadConfig covers the validation half of R6: a non-array value and an
// empty array must both be rejected with a clear error, never silently tried as a shell string.
func TestKeyCommandArgvRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
	}{
		{"not_json", "helper --label vault"},
		{"json_string_not_array", `"helper"`},
		{"empty_array", `[]`},
		{"empty_executable", `["", "x"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseKeyCommandArgv(tc.raw); err == nil {
				t.Fatalf("parseKeyCommandArgv(%q) succeeded, want an error", tc.raw)
			}
		})
	}
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
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	type child struct {
		cmd    *exec.Cmd
		output strings.Builder
		ready  string
	}
	children := make([]*child, 4)
	stop := filepath.Join(t.TempDir(), "stop")
	for i := range children {
		c := &child{
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
		for {
			if _, err := os.Stat(c.ready); err == nil {
				break
			} else if !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatal("reader subprocess never became ready")
			}
			time.Sleep(time.Millisecond)
		}
	}
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

// TestParseKeyCommandArgvAllowsEmptyArgument covers the review's LOW #2: a valid argv may contain
// an empty argument after the executable at index 0 — only the executable itself must be nonempty.
func TestParseKeyCommandArgvAllowsEmptyArgument(t *testing.T) {
	want := []string{"helper", "--label", ""}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseKeyCommandArgv(string(raw))
	if err != nil {
		t.Fatalf("parseKeyCommandArgv(%q) = %v, want the exact argv round-tripped", raw, err)
	}
	if len(got) != len(want) {
		t.Fatalf("parseKeyCommandArgv = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseKeyCommandArgv = %#v, want %#v", got, want)
		}
	}
}
