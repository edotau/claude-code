package vault

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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
	if len(ring.values) != 2 {
		t.Errorf("keyring has %d accounts, want exactly the two vault accounts", len(ring.values))
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

func TestRotatePendingRecoveredFirst(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "committed-before-promotion")
	old, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	next, nextValue, err := randomKey()
	if err != nil {
		t.Fatal(err)
	}
	acct := "vault-key/" + phase1bMeta(t, "vault_id")
	ring.mu.Lock()
	oldValue := ring.values[acct]
	ring.values[acct+"."+old.Kid] = oldValue
	ring.values[acct+".next"] = nextValue
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
	ring.mu.Lock()
	before := ring.sets
	ring.mu.Unlock()
	_, gotKid, err := RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	ring.mu.Lock()
	sets, current, pending := ring.sets-before, ring.values[acct], ring.values[acct+".next"]
	retained := ring.values[acct+"."+old.Kid]
	ring.mu.Unlock()
	if sets != 1 || current != nextValue || pending != "" || gotKid != next.Kid || retained != oldValue {
		t.Fatalf("pending recovery generated/overwrote a key: sets=%d promoted=%t pending=%t kid=%q retained=%t",
			sets, current == nextValue, pending != "", gotKid, retained == oldValue)
	}
	if kid := phase1bMeta(t, "kid"); kid != next.Kid {
		t.Fatalf("recovery changed committed kid to %q, want %q", kid, next.Kid)
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
		record, ok := tx.Record("RACE")
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
	acct := "vault-key/" + phase1bMeta(t, "vault_id")
	oldKid := phase1bMeta(t, "kid")
	next, nextValue, err := randomKey()
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

func TestNestedOpenErrors(t *testing.T) {
	phase1bRing(t)
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
			case <-time.After(6 * time.Second):
				t.Fatal("nested open hung beyond bbolt timeout")
			}
			if !errors.Is(nested, errNested) || elapsed >= 100*time.Millisecond {
				t.Fatalf("nested %s = %v after %s, want errNested within 100ms", operation, nested, elapsed)
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
		record, ok := tx.Record("TOKEN")
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
	acct := "vault-key/" + phase1bMeta(t, "vault_id")
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
	}
	pending := ring.values[acct+".next"]
	ring.mu.Unlock()
	current, err := parseKey(currentValue)
	if err != nil || current.Kid != finalKid || pending != "" {
		t.Fatalf("current key/meta mismatch or pending key left: parse=%v meta=%q pending=%t", err, finalKid, pending != "")
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
			return fmt.Errorf("snapshot missing records or metadata bucket")
		}
		count, err := strconv.Atoi(string(meta.Get([]byte("record_count"))))
		if err != nil || count < base || records.Stats().KeyN != count {
			return fmt.Errorf("snapshot is not a committed generation: count=%d records=%d err=%v",
				count, records.Stats().KeyN, err)
		}
		for i := range count {
			r, ok := (&Tx{tx}).Record(fmt.Sprintf("TOKEN_%03d", i))
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

func TestKeyCommandArgvNoShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable; production argv behavior is platform independent")
	}
	phase1bRing(t)
	key, raw, err := randomKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	program, argsFile := filepath.Join(dir, "key-command"), filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$#\" \"$@\" > \"$ARGV_FILE\"\nprintf '%s\\n' \"$KEY_VALUE\"\n"
	if err := os.WriteFile(program, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARGV_FILE", argsFile)
	t.Setenv("KEY_VALUE", raw)
	if err := Update(func(tx *Tx) error {
		return tx.Put(Meta, "key_command", program+" literal;value")
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
	if string(args) != "1\nliteral;value\n" {
		t.Fatalf("key_command used shell parsing: argv capture %q, want one literal semicolon argument", args)
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
	deadline := time.Now().Add(3 * time.Second)
	reads := 0
	for time.Now().Before(deadline) {
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
	for i := range children {
		c := &child{
			cmd:   exec.Command(executable, "-test.run=^TestVaultReaderHelper$", "-test.timeout=15s"),
			ready: filepath.Join(t.TempDir(), "ready"),
		}
		c.cmd.Env = append(os.Environ(), "PHASE1B_READER_HELPER=1", "PHASE1B_READER_READY="+c.ready)
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
	for _, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Errorf("reader subprocess: %v\n%s", err, c.output.String())
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p95 := latencies[(95*len(latencies)+99)/100-1]
	t.Logf("30 writer latencies: %v; p95=%s", latencies, p95)
	if p95 >= 500*time.Millisecond {
		t.Fatalf("writer p95 %s exceeds spec limit 500ms", p95)
	}
}
