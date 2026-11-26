package memory

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBankStoreLoadReturnsSixDocs(t *testing.T) {
	batchStore(t)
	docs, _, err := newBankStore(false).Load()
	if err != nil || len(docs) != len(bankFiles) {
		t.Fatalf("docs=%d err=%v", len(docs), err)
	}
}

func TestBankStoreApplyWritesAdd(t *testing.T) {
	batchStore(t)
	rs, err := newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().Now)
	if err != nil || len(rs) != 1 || rs[0].Outcome != DreamOK {
		t.Fatalf("rs=%+v err=%v", rs, err)
	}
	if got := mustRead(t, Path("decisionLog.md")); !strings.Contains(got, " — n <!-- id:") {
		t.Errorf("not written:\n%s", got)
	}
}

func TestBankStoreApplyLocked(t *testing.T) {
	batchStore(t)
	err := withFlatLock(BankLockWait, func() error {
		_, err := newBankStore(true).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().Now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

type fakeDBStore struct {
	loads, applies, closed int
	base                   Versions
	opErr, closeErr        error
}

func (f *fakeDBStore) Load() (map[string]Doc, Versions, error) { f.loads++; return nil, nil, f.opErr }
func (f *fakeDBStore) Apply(_ Batch, base Versions, _ time.Time) ([]OpResult, error) {
	f.applies++
	f.base = base
	return []OpResult{{Outcome: DreamOK}}, f.opErr
}

func truthEnv(t *testing.T, f *fakeDBStore, openErr error) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	oldOpen, oldExp := OpenBankStore, ExportBeforeFlip
	t.Cleanup(func() { OpenBankStore, ExportBeforeFlip = oldOpen, oldExp })
	OpenBankStore, ExportBeforeFlip = nil, nil
	if f != nil {
		OpenBankStore = func(bool) (BankStore, func() error, error) {
			if openErr != nil {
				return nil, nil, openErr
			}
			return f, func() error { f.closed++; return f.closeErr }, nil
		}
	}
}

func TestTruthDefault(t *testing.T) {
	truthEnv(t, &fakeDBStore{}, nil)
	if got, err := TruthErr(); got != "db" || err != nil {
		t.Fatalf("linked store: TruthErr() = %q, %v", got, err)
	}
	truthEnv(t, nil, nil)
	if got, err := TruthErr(); got != "files" || err != nil {
		t.Fatalf("no store: TruthErr() = %q, %v", got, err)
	}
	write(t, truthPath(), "bogus\n")
	if _, err := TruthErr(); err == nil {
		t.Fatal("unknown value read without error")
	}
	if got := Truth(); got != "files" {
		t.Fatalf("Truth() = %q", got)
	}
}

func TestSetTruthRoundTrip(t *testing.T) {
	truthEnv(t, nil, nil)
	flips := 0
	ExportBeforeFlip = func() error { flips++; return nil }
	for _, want := range []string{"db", "files"} {
		if err := SetTruth(want); err != nil || Truth() != want {
			t.Fatalf("SetTruth(%q): err=%v Truth=%q", want, err, Truth())
		}
	}
	if flips != 1 {
		t.Errorf("export ran %d times, want 1 (leaving db only)", flips)
	}
	if err := SetTruth("sql"); err == nil {
		t.Error("invalid truth accepted")
	}
}

func TestSetTruthFlipBackNeedsExport(t *testing.T) {
	truthEnv(t, nil, nil)
	if err := SetTruth("db"); err != nil {
		t.Fatal(err)
	}
	if err := SetTruth("files"); err == nil || Truth() != "db" {
		t.Fatalf("flip without exporter: err=%v truth=%q", err, Truth())
	}
	ExportBeforeFlip = func() error { return errors.New("boom") }
	if err := SetTruth("files"); err == nil || Truth() != "db" {
		t.Fatalf("flip with failing exporter: err=%v truth=%q", err, Truth())
	}
	write(t, truthPath(), "bogus\n")
	ExportBeforeFlip = func() error { return nil }
	if err := SetTruth("files"); err == nil {
		t.Fatal("SetTruth overwrote an unknown state file")
	}
}

func TestNewBankStoreFailsClosedUnderDB(t *testing.T) {
	truthEnv(t, nil, nil)
	if err := SetTruth("db"); err != nil {
		t.Fatal(err)
	}
	s := newBankStore(false)
	if _, _, err := s.Load(); err == nil {
		t.Error("Load with no opener succeeded")
	}
	if _, err := s.Apply(Batch{}, nil, time.Now()); err == nil {
		t.Error("Apply with no opener succeeded")
	}
	write(t, truthPath(), "bogus\n")
	if _, _, err := newBankStore(false).Load(); err == nil {
		t.Error("unknown truth did not fail closed")
	}
}

func TestApplyFollowsFlipAfterSelection(t *testing.T) {
	f := &fakeDBStore{}
	truthEnv(t, f, nil)
	batchStore(t)
	s := newBankStore(false)
	if err := SetTruth("db"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().Now); err != nil || f.applies != 1 {
		t.Fatalf("err=%v applies=%d", err, f.applies)
	}
	if got := mustRead(t, Path("decisionLog.md")); strings.Contains(got, " — n <!-- id:") {
		t.Errorf("stale file store wrote:\n%s", got)
	}
}

func TestNewBankStoreOpenerFailureNeverWritesFiles(t *testing.T) {
	truthEnv(t, &fakeDBStore{}, errors.New("locked out"))
	batchStore(t)
	if err := SetTruth("db"); err != nil {
		t.Fatal(err)
	}
	if _, err := newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().Now); err == nil {
		t.Fatal("Apply succeeded")
	}
	if got := mustRead(t, Path("decisionLog.md")); strings.Contains(got, " — n <!-- id:") {
		t.Errorf("files written under db truth:\n%s", got)
	}
}

func TestNewBankStoreUsesOpener(t *testing.T) {
	f := &fakeDBStore{}
	truthEnv(t, f, nil)
	if err := SetTruth("db"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newBankStore(false).Load(); err != nil || f.loads != 1 || f.closed != 1 {
		t.Fatalf("err=%v loads=%d closed=%d", err, f.loads, f.closed)
	}
	base := Versions{"r": "v"}
	rs, err := newBankStore(true).Apply(Batch{}, base, time.Now())
	if err != nil || len(rs) != 1 {
		t.Fatalf("rs=%v err=%v base=%v", rs, err, f.base)
	}
	if f.applies != 1 || f.closed != 2 || f.base["r"] != "v" {
		t.Fatalf("applies=%d closed=%d base=%v", f.applies, f.closed, f.base)
	}
}

func TestDBStoreCloseAndOpErrors(t *testing.T) {
	f := &fakeDBStore{closeErr: errors.New("close")}
	truthEnv(t, f, nil)
	if err := SetTruth("db"); err != nil {
		t.Fatal(err)
	}
	if _, err := newBankStore(false).Apply(Batch{}, nil, time.Now()); err != nil {
		t.Fatalf("close failure failed a committed batch: %v", err)
	}
	f.opErr = errors.New("conflict")
	if _, err := newBankStore(false).Apply(Batch{}, nil, time.Now()); !errors.Is(err, f.opErr) || f.closed != 2 {
		t.Fatalf("err=%v closed=%d", err, f.closed)
	}
}

func TestBankStoreLoadVersions(t *testing.T) {
	batchStore(t)
	write(t, Path("progress.md"), "# Progress\n\n## In Progress\n\n- Dup\n- Dup\n- Solo\n")
	_, v, err := newBankStore(false).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v[contentRef("- Dup")]; !ok || got != "" {
		t.Errorf("duplicate ref version = %q, %v; want empty", got, ok)
	}
	if v[contentRef("- Solo")] == "" {
		t.Error("unique ref has no version")
	}
}

// docDBStore is a db store over fakeExportDB: Apply writes the db only, the export belongs to the production path.
type docDBStore struct {
	db      *fakeExportDB
	applied atomic.Bool
}

// Load returns docs only; version rules belong to the real store (shadow.TestBankStoreLoadAmbiguousRefHasNoVersion).
func (s *docDBStore) Load() (map[string]Doc, Versions, error) {
	docs := map[string]Doc{}
	for _, name := range bankFiles {
		docs[name] = ParseDoc(name, []byte(s.db.docs[name]))
	}
	return docs, nil, nil
}

func (s *docDBStore) Apply(b Batch, base Versions, now time.Time) ([]OpResult, error) {
	b, err := withBase(b, base)
	if err != nil {
		return nil, err
	}
	docs, _, _ := s.Load()
	out, rs, err := ApplyDocs(docs, b, now)
	if err != nil {
		return rs, err
	}
	for _, name := range bankFiles {
		s.db.docs[name] = string(out[name].Render())
	}
	s.applied.Store(true)
	return rs, nil
}

// dbBank is a bank in db truth whose decisionLog and conventions are batchStore's fixtures, imported into the db.
func dbBank(t *testing.T) (*fakeExportDB, *docDBStore) {
	t.Helper()
	t.Setenv("HARNESS_KEYRING", "off")
	db := dbTruthBank(t)
	write(t, Path("decisionLog.md"), datedIn)
	write(t, Path("conventions.md"), bulletsIn)
	exportGuard(t, db)
	st := &docDBStore{db: db}
	old := OpenBankStore
	OpenBankStore = func(bool) (BankStore, func() error, error) { return st, func() error { return nil }, nil }
	t.Cleanup(func() { OpenBankStore = old })
	return db, st
}

func TestDBStoreApplyTakesBankLock(t *testing.T) {
	_, st := dbBank(t)
	shortBankWait(t) // a held lock makes Apply fail after the wait, so no sleep decides the result
	var err error
	if lerr := withFlatLock(BankLockWait, func() error {
		_, err = newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().Now)
		return nil
	}); lerr != nil {
		t.Fatal(lerr)
	}
	if err == nil || st.applied.Load() {
		t.Fatalf("Apply under a held bank lock: err=%v applied=%v", err, st.applied.Load())
	}
	if _, err := newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().Now); err != nil || !st.applied.Load() {
		t.Fatalf("Apply after release: err=%v applied=%v", err, st.applied.Load())
	}
}

func TestApplyTypedDBExportFailureKeepsBatch(t *testing.T) {
	for name, fail := range map[string]func(db *fakeExportDB){
		"open": func(*fakeExportDB) {
			OpenExportDB = func() (ExportDB, func() error, error) { return nil, nil, errors.New("export down") }
		},
		"export": func(db *fakeExportDB) { db.hook = func(op, _ string) error { return errors.New("render down") } },
	} {
		t.Run(name, func(t *testing.T) {
			db, _ := dbBank(t)
			old := OpenExportDB
			t.Cleanup(func() { OpenExportDB = old })
			fail(db)
			rs, err := newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().Now)
			if len(rs) != 1 || rs[0].Outcome != DreamOK {
				t.Errorf("results = %+v, want the op applied", rs)
			}
			if !strings.Contains(db.docs["decisionLog.md"], " — n <!-- id:") {
				t.Errorf("the committed row was undone:\n%s", db.docs["decisionLog.md"])
			}
			_, committed := errors.AsType[*committedErr](err)
			_, after := errors.AsType[*AfterCommitError](err) // committed: never retried, never sent to fallback
			if !committed || !after {
				t.Errorf("want a confirmed after-commit error, got %v", err)
			}
		})
	}
}

func TestApplyTypedDBOutsideEditKept(t *testing.T) {
	db, _ := dbBank(t)
	write(t, Path("activeContext.md"), "# Active Context\n\n- first\n") // the guard imports it, then the hook edits another file
	fired := false
	db.hook = func(op, rel string) error {
		if op == "import" && rel == "activeContext.md" && !fired {
			fired = true
			write(t, Path("conventions.md"), bulletsIn+"\n- outside\n")
		}
		return nil
	}
	if _, err := newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().Now); err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("the guard never imported the first edit, so no edit landed inside the export")
	}
	if got := mustRead(t, Path("conventions.md")); !strings.Contains(got, "- outside") {
		t.Errorf("the outside edit was overwritten:\n%s", got)
	}
	if !strings.Contains(db.docs["conventions.md"], "- outside") {
		t.Errorf("the outside edit never reached the db:\n%s", db.docs["conventions.md"])
	}
	if got := mustRead(t, Path("decisionLog.md")); !strings.Contains(got, " — n <!-- id:") {
		t.Errorf("the typed apply is not exported:\n%s", got)
	}
}

func TestDBStoreApplyRefusesAmbiguousRef(t *testing.T) {
	db, _ := dbBank(t)
	db.docs["progress.md"] = "# Progress\n\n## In Progress\n\n- Dup\n- Dup\n- Solo\n"
	before := db.docs["progress.md"]
	dup := contentRef("- Dup")
	// Load gives a duplicate ref the empty version
	upd := Op{Op: "update", ID: dup, Text: "x"}
	if _, err := newBankStore(false).Apply(Batch{Ops: []Op{upd}}, Versions{dup: ""}, testEnv().Now); err == nil || db.docs["progress.md"] != before {
		t.Errorf("an ambiguous ref was written: err=%v\n%s", err, db.docs["progress.md"])
	}
}

func TestWithBaseFillsAndRefusesRead(t *testing.T) {
	base := Versions{"0a1b2c3d": "z11111111", "skip": ""}
	b := Batch{Ops: []Op{{Op: "update", ID: "0a1b2c3d", Text: "x"}, {Op: "update", ID: "skip", Text: "y"}}}
	got, err := withBase(b, base)
	if err != nil || got.Ops[0].Read != "z11111111" || got.Ops[1].Read != "" {
		t.Fatalf("fill: %+v err=%v", got.Ops, err)
	}
	if b.Ops[0].Read != "" {
		t.Error("withBase modified its input")
	}
	b.Ops[0].Read = "z11111111"
	if _, err := withBase(b, base); err != nil {
		t.Errorf("a read equal to base was refused: %v", err)
	}
	b.Ops[0].Read = "z00000000"
	if _, err := withBase(b, base); err == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Errorf("a read that conflicts with base: err=%v", err)
	}
	// no base entry means no guard: the caller's Read stays and the store decides
	nb := Batch{Ops: []Op{{Op: "update", ID: "absent", Read: "z9", Text: "x"}}}
	if got, err := withBase(nb, base); err != nil || got.Ops[0].Read != "z9" {
		t.Errorf("a read with no base entry: %+v err=%v", got.Ops, err)
	}
}
