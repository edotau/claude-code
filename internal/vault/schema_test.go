package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/schemas"
	"go.etcd.io/bbolt"
)

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
