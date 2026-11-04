package vault

import (
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

	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

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
	commitResealed(t, next, "TOKEN", "committed-before-promotion")
	// Before any further rotation even runs, a plain read must already recover via the per-kid
	// account — no "promotion" step is required for K1 to survive.
	invalidateKey()
	phase1bRead(t, "TOKEN", "committed-before-promotion")

	_, gotKid, err := RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	current := ringValue(ring, acct)
	retainedOld, retainedNext := ringValue(ring, acct+"."+old.Kid), ringValue(ring, acct+"."+next.Kid)
	if current == "" || gotKid == "" || retainedOld != oldValue || retainedNext != nextValue {
		t.Fatalf("rotation after a crashed promotion lost a key: current=%t kid=%q retainedOld=%t retainedNext=%t",
			current != "", gotKid, retainedOld == oldValue, retainedNext == nextValue)
	}
	if kid := phase1bMeta(t, "kid"); kid != gotKid {
		t.Fatalf("recovery changed committed kid to %q, want %q", kid, gotKid)
	}
	phase1bRead(t, "TOKEN", "committed-before-promotion")
}

// commitResealed commits name sealed under k and advances meta/kid to k, as a rotation's reseal tx does.
func commitResealed(t *testing.T, k *Key, name, value string) {
	t.Helper()
	if err := Update(func(tx *Tx) error {
		if err := tx.PutRecord(name, Record{
			Sealed: k.Seal(name, value), Kid: k.Kid,
		}); err != nil {
			return err
		}
		return tx.Put(Meta, "kid", k.Kid)
	}); err != nil {
		t.Fatal(err)
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
