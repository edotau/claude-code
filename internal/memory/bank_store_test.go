package memory

import (
	"errors"
	"strings"
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

func TestTruthDefaultsToFiles(t *testing.T) {
	truthEnv(t, nil, nil)
	if got, err := TruthErr(); got != "files" || err != nil {
		t.Fatalf("TruthErr() = %q, %v", got, err)
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
