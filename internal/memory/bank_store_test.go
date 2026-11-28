package memory

import (
	"bytes"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

// memTestStore stands in for the shadow store (an import cycle bars it): import files, ApplyDocs, export; no settle.
type memTestStore struct{}

func init() {
	OpenBankStore = func(bool) (BankStore, func() error, error) { return memTestStore{}, func() error { return nil }, nil }
}

func readBankDocs() (map[string]Doc, map[string][]byte, error) {
	docs, raw := map[string]Doc{}, map[string][]byte{}
	for _, name := range bankFiles {
		data, err := os.ReadFile(Path(name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
		raw[name], docs[name] = data, ParseDoc(name, data)
	}
	return docs, raw, nil
}

// memVersions issues numeric tokens like the db store's row versions, never equal to a content ref.
func memVersions(docs map[string]Doc) Versions {
	versions := Versions{}
	for ref, content := range contentRefs(docs) {
		if content == "" {
			versions[ref] = ""
			continue
		}
		h := fnv.New32a()
		h.Write([]byte(content))
		versions[ref] = Version(strconv.FormatUint(uint64(h.Sum32()), 10))
	}
	return versions
}

func (memTestStore) Load() (map[string]Doc, Versions, error) {
	docs, _, err := readBankDocs()
	if err != nil {
		return nil, nil, err
	}
	return docs, memVersions(docs), nil
}

// memCheckBase mirrors shadow's checkBase: an op naming a ref whose current version is not its base conflicts.
func memCheckBase(b Batch, base, cur Versions) error {
	for i, o := range b.Ops {
		have, want := cur[o.ID], base[o.ID]
		if !memBaseConflicts(o, have, want, cur) {
			continue
		}
		return &ConflictError{Op: i, Ref: o.ID, Base: want, Current: have}
	}
	return nil
}

func memBaseConflicts(o Op, have, want Version, cur Versions) bool {
	if _, known := cur[o.ID]; o.ID == "" || (!known && want == "") || (want != "" && have == want) {
		return false
	}
	unchecked := want == "" && (o.Read != "" || strings.HasPrefix(o.ID, "z"))
	return !unchecked || have == ""
}

func (memTestStore) Apply(b Batch, base Versions, now time.Time) ([]OpResult, error) {
	docs, raw, err := readBankDocs()
	if err != nil {
		return nil, err
	}
	if err := memCheckBase(b, base, memVersions(docs)); err != nil {
		return nil, err
	}
	if b, err = withBase(b, contentRefs(docs)); err != nil { // the version is opaque; Read guards on content
		return nil, err
	}
	docs, results, err := ApplyDocs(docs, b, now)
	if err != nil {
		return results, err
	}
	for _, name := range bankFiles {
		if out := docs[name].Render(); !bytes.Equal(out, raw[name]) {
			if err := paths.AtomicWrite(Path(name), out, 0o644); err != nil {
				return results, err
			}
		}
	}
	return results, commitBanks("test: export") // the real export commits what it writes
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

// linkStore points OpenBankStore at f (nil: no store linked) for the test; opened runs on each open.
func linkStore(t *testing.T, f *fakeDBStore, openErr error, opened func()) {
	t.Helper()
	old := OpenBankStore
	t.Cleanup(func() { OpenBankStore = old })
	OpenBankStore = nil
	if f == nil {
		return
	}
	OpenBankStore = func(bool) (BankStore, func() error, error) {
		if opened != nil {
			opened()
		}
		if openErr != nil {
			return nil, nil, openErr
		}
		return f, func() error { f.closed++; return f.closeErr }, nil
	}
}

func TestNewBankStoreFailsClosedWithNoStore(t *testing.T) {
	tempConfig(t)
	linkStore(t, nil, nil, nil)
	s := newBankStore(false)
	if _, _, err := s.Load(); !errors.Is(err, errNoDBStore) {
		t.Errorf("Load with no opener = %v", err)
	}
	if _, err := s.Apply(Batch{}, nil, time.Now()); err == nil {
		t.Error("Apply with no opener succeeded")
	}
}

func TestNewBankStoreOpenerFailureNeverWritesFiles(t *testing.T) {
	batchStore(t)
	linkStore(t, &fakeDBStore{}, errors.New("locked out"), nil)
	if _, err := newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().now); err == nil {
		t.Fatal("Apply succeeded")
	}
	if got := mustRead(t, Path("decisionLog.md")); strings.Contains(got, " — n <!-- id:") {
		t.Errorf("files written after a failed open:\n%s", got)
	}
}

func TestNewBankStoreUsesOpener(t *testing.T) {
	batchStore(t)
	f := &fakeDBStore{}
	linkStore(t, f, nil, nil)
	if _, _, err := newBankStore(false).Load(); err != nil || f.loads != 1 || f.closed != 1 {
		t.Fatalf("err=%v loads=%d closed=%d", err, f.loads, f.closed)
	}
	base := Versions{"r": "v"}
	rs, err := newBankStore(false).Apply(Batch{}, base, time.Now())
	if err != nil || len(rs) != 1 {
		t.Fatalf("rs=%v err=%v base=%v", rs, err, f.base)
	}
	if f.applies != 1 || f.closed != 2 || f.base["r"] != "v" {
		t.Fatalf("applies=%d closed=%d base=%v", f.applies, f.closed, f.base)
	}
}

func TestDBStoreCloseAndOpErrors(t *testing.T) {
	batchStore(t)
	f := &fakeDBStore{closeErr: errors.New("close")}
	linkStore(t, f, nil, nil)
	if _, err := newBankStore(false).Apply(Batch{}, nil, time.Now()); err != nil {
		t.Fatalf("close failure failed a committed batch: %v", err)
	}
	f.opErr = errors.New("conflict")
	if _, err := newBankStore(false).Apply(Batch{}, nil, time.Now()); !errors.Is(err, f.opErr) || f.closed != 2 {
		t.Fatalf("err=%v closed=%d", err, f.closed)
	}
}

// A caller that holds the lock passes locked=true: Apply must not take it again (flock is not reentrant).
func TestDBStoreApplyLockedDoesNotRelock(t *testing.T) {
	batchStore(t)
	f := &fakeDBStore{}
	linkStore(t, f, nil, nil)
	done := make(chan error, 1)
	go func() {
		done <- WithBankLock(BankLockWait, func() error {
			_, err := newBankStore(true).Apply(Batch{}, nil, time.Now())
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil || f.applies != 1 {
			t.Fatalf("err=%v applies=%d", err, f.applies)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a locked Apply deadlocked on the bank lock")
	}
}

// A locked Apply runs the export under the caller's lock: a relock in exportAfterApply or ExportGuardLocked hangs here.
func TestDBStoreApplyLockedExportDoesNotRelock(t *testing.T) {
	dbTruthBank(t)
	opens := 0
	inner := OpenExportDB
	OpenExportDB = func() (ExportDB, func() error, error) { opens++; return inner() }
	linkStore(t, &fakeDBStore{}, nil, nil)
	done := make(chan error, 1)
	go func() {
		done <- WithBankLock(BankLockWait, func() error {
			_, err := newBankStore(true).Apply(Batch{}, nil, time.Now())
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil || opens != 1 {
			t.Fatalf("err=%v export opens=%d, want one export", err, opens)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a locked Apply deadlocked in the export")
	}
}

// H-2: a failed Markdown export after the db commit leaves the batch committed and reports it as applied.
func TestDBStoreApplyExportFailureKeepsBatch(t *testing.T) {
	batchStore(t)
	f := &fakeDBStore{}
	linkStore(t, f, nil, nil)
	old := OpenExportDB
	t.Cleanup(func() { OpenExportDB = old })
	OpenExportDB = func() (ExportDB, func() error, error) { return nil, nil, errors.New("export db down") }
	rs, err := newBankStore(false).Apply(Batch{}, nil, time.Now())
	if _, after := errors.AsType[*afterCommitError](err); !after || refused(err) || f.applies != 1 || len(rs) != 1 {
		t.Fatalf("err=%v applies=%d rs=%v; want the commit kept and an after-commit error", err, f.applies, rs)
	}
}

// linkExport wires a fake export db whose guard imports the bank files; it is restored by t.Cleanup.
func linkExport(t *testing.T) {
	t.Helper()
	db := &fakeExportDB{root: paths.MemoryStoreDir(), docs: map[string]string{}, last: map[string]string{}}
	old := OpenExportDB
	t.Cleanup(func() { OpenExportDB = old })
	OpenExportDB = func() (ExportDB, func() error, error) { return db, func() error { return nil }, nil }
}

func applyOnce(t *testing.T) error {
	t.Helper()
	_, err := newBankStore(false).Apply(Batch{}, nil, time.Now())
	return err
}

// H-6: a stored "files" truth reads as db only after the open and a clean guard import, never on a Load alone.
func TestFirstOpenAfterTruthFilesImportsFirst(t *testing.T) {
	batchStore(t)
	linkExport(t)
	write(t, truthPath(), "files\n")
	var atOpen string
	linkStore(t, &fakeDBStore{}, nil, func() { atOpen = strings.TrimSpace(mustRead(t, truthPath())) })
	if _, _, err := newBankStore(false).Load(); err != nil || strings.TrimSpace(mustRead(t, truthPath())) != "files" {
		t.Fatalf("Load = %v, truth %q; want no retire on a read", err, mustRead(t, truthPath()))
	}
	if err := applyOnce(t); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(mustRead(t, truthPath())); atOpen != "files" || got != "db" {
		t.Fatalf("truth at open %q, after %q; want files, then db", atOpen, got)
	}
}

func TestFilesTruthKeptWhenOpenFails(t *testing.T) {
	batchStore(t)
	linkExport(t)
	write(t, truthPath(), "files\n")
	linkStore(t, &fakeDBStore{}, errors.New("import failed"), nil)
	if err := applyOnce(t); err == nil {
		t.Fatal("Apply succeeded")
	}
	if got := strings.TrimSpace(mustRead(t, truthPath())); got != "files" {
		t.Fatalf("truth = %q after a failed import, want files kept", got)
	}
}

func TestFilesTruthKeptWhenExportUnavailable(t *testing.T) {
	batchStore(t)
	write(t, truthPath(), "files\n")
	linkStore(t, &fakeDBStore{}, nil, nil)
	old := OpenExportDB
	t.Cleanup(func() { OpenExportDB = old })
	OpenExportDB = nil // no guard ran, so no import is proven
	if err := applyOnce(t); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(mustRead(t, truthPath())); got != "files" {
		t.Fatalf("truth = %q with no guard pass, want files kept", got)
	}
}

func TestUnknownTruthValueRetiresToDB(t *testing.T) {
	batchStore(t)
	linkExport(t)
	write(t, truthPath(), "bogus\n")
	linkStore(t, &fakeDBStore{}, nil, nil)
	if err := applyOnce(t); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(mustRead(t, truthPath())); got != "db" {
		t.Fatalf("truth = %q, want db", got)
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
	docs, _, _ := s.Load()
	b, err := withBase(b, contentRefs(docs))
	if err != nil {
		return nil, err
	}
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
	if lerr := withBankLock(BankLockWait, func() error {
		_, err = newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().now)
		return nil
	}); lerr != nil {
		t.Fatal(lerr)
	}
	if err == nil || st.applied.Load() {
		t.Fatalf("Apply under a held bank lock: err=%v applied=%v", err, st.applied.Load())
	}
	if _, err := newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().now); err != nil || !st.applied.Load() {
		t.Fatalf("Apply after release: err=%v applied=%v", err, st.applied.Load())
	}
}

func TestApplyTypedDBWritesChangedFilesOnly(t *testing.T) {
	db, _ := dbBank(t)
	decision, conventions := db.docs["decisionLog.md"], db.docs["conventions.md"]
	bad := Op{Op: "update", ID: "ffffffff", Read: "z00000000", Text: "x"}
	store := newBankStore(false)
	if _, err := store.Apply(Batch{Ops: []Op{batchAdd, bad}}, nil, testEnv().now); err == nil {
		t.Fatal("a failing op must refuse the whole batch")
	}
	if db.docs["decisionLog.md"] != decision || db.docs["conventions.md"] != conventions ||
		mustRead(t, Path("decisionLog.md")) != decision || mustRead(t, Path("conventions.md")) != conventions {
		t.Fatal("a refused batch changed the db or its export")
	}
	rs, err := store.Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().now)
	if err != nil || len(rs) != 1 || rs[0].Outcome != DreamOK {
		t.Fatalf("rs=%+v err=%v", rs, err)
	}
	if got := mustRead(t, Path("decisionLog.md")); got != db.docs["decisionLog.md"] || !strings.Contains(got, " — n <!-- id:") {
		t.Fatalf("committed entry was not exported: %q", got)
	}
	if db.docs["conventions.md"] != conventions || mustRead(t, Path("conventions.md")) != conventions {
		t.Fatal("an unrelated document changed")
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
			rs, err := newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().now)
			if len(rs) != 1 || rs[0].Outcome != DreamOK {
				t.Errorf("results = %+v, want the op applied", rs)
			}
			if !strings.Contains(db.docs["decisionLog.md"], " — n <!-- id:") {
				t.Errorf("the committed row was undone:\n%s", db.docs["decisionLog.md"])
			}
			_, committed := errors.AsType[*committedErr](err)
			_, after := errors.AsType[*afterCommitError](err) // committed: never retried, never sent to fallback
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
	if _, err := newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().now); err != nil {
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
	if _, err := newBankStore(false).Apply(Batch{Ops: []Op{upd}}, Versions{dup: ""}, testEnv().now); err == nil || db.docs["progress.md"] != before {
		t.Errorf("an ambiguous ref was written: err=%v\n%s", err, db.docs["progress.md"])
	}
}

func TestWithBaseFillsAndRefusesRead(t *testing.T) {
	docs := map[string]Doc{"progress.md": ParseDoc("progress.md", []byte("# Progress\n\n- Rule <!-- id:0a1b2c3d -->\n"))}
	ref := contentRef(docs["progress.md"].Entries[0].block())
	b := Batch{Ops: []Op{{Op: "update", ID: "0a1b2c3d", Text: "x"}}}
	got, err := withBase(b, contentRefs(docs))
	if err != nil || got.Ops[0].Read != ref {
		t.Fatalf("fill: %+v err=%v", got.Ops, err)
	}
	if b.Ops[0].Read != "" {
		t.Error("withBase modified its input")
	}
	b.Ops[0].Read = ref
	if _, err := withBase(b, contentRefs(docs)); err != nil {
		t.Errorf("a read equal to the content ref was refused: %v", err)
	}
	b.Ops[0].Read = "z00000000"
	if _, err := withBase(b, contentRefs(docs)); err == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Errorf("a read that conflicts with content: err=%v", err)
	}
	nb := Batch{Ops: []Op{{Op: "update", ID: "absent", Read: "z9", Text: "x"}}}
	if got, err := withBase(nb, contentRefs(docs)); err != nil || got.Ops[0].Read != "z9" {
		t.Errorf("a read with no entry: %+v err=%v", got.Ops, err)
	}
}

func TestArchiveSource(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		wantSrc string
		wantOK  bool
	}{
		{"month", "conventions-2026-10.md", "conventions", true},
		{"dream", "decisionLog-2026-11-20-dream.md", "decisionLog", true},
		{"trim", "progress-2026-11-20-trim.md", "progress", true},
		{"focus", "activeContext-2026-11-20-focus.md", "activeContext", true},
		{"repo slug", "decisionLog-2026-11-19-sample-repo.md", "decisionLog", true},
		{"month repo slug", "sessionHistory-2026-08-digitalbtd-rapid.md", "sessionHistory", true},
		{"month one word", "sessionHistory-2026-08-digitalbtd.md", "sessionHistory", true},
		{"src archive", "conventions-archive.md", "conventions", true},
		{"merged no source", "merged-claude-code-progress.md", "", false},
		{"overflow", "2026-11-18-decisionLog-overflow.md", "decisionLog", true},
		{"overflow repo slug", "2026-11-18-sessionHistory-overflow-sample-repo.md", "sessionHistory", true},
		{"unplaceable gemini", "2026-10-21-gemini-history-snapshot.md", "", false},
		{"unplaceable harness", "2026-11-01-harness-progress-snapshot.md", "", false},
		{"invented source month", "invented-2026-10.md", "", false},
		{"invented source overflow", "2026-11-18-invented-overflow.md", "", false},
		{"invented source slug", "invented-2026-11-19-repo.md", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, ok := ArchiveSource(tt.file)
			if src != tt.wantSrc || ok != tt.wantOK {
				t.Errorf("ArchiveSource(%q) = %q, %v; want %q, %v", tt.file, src, ok, tt.wantSrc, tt.wantOK)
			}
		})
	}
}

func TestGrammarOfArchiveUsesSource(t *testing.T) {
	for name, want := range map[string]Grammar{
		"archive/2026-11-01-decisionLog-overflow.md": GrammarDated,
		"archive/conventions-2026-11-20-trim.md":     GrammarBullets,
		"archive/progress-2026-11.md":                GrammarSections,
		"archive/conventions-archive.md":             GrammarBullets,
		"archive/2026-10-21-gemini-snapshot.md":      GrammarSections,
	} {
		if got := grammarOf(name); got != want {
			t.Errorf("GrammarOf(%q) = %v, want %v", name, got, want)
		}
	}
}

// wantFile asserts path holds want; an empty want means the file must not exist.
func wantFile(t *testing.T, path, want string) {
	t.Helper()
	if !pathExists(path) {
		if want != "" {
			t.Errorf("%s missing", path)
		}
		return
	}
	if got := aliasRead(t, path); got != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

func aliasRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func pathExists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestBankPaths(t *testing.T) {
	isolatedConfig(t)
	s := paths.MemoryStoreDir()
	for got, want := range map[string]string{
		Path("activeContext.md"):               "activeContext.md",
		paths.MemorySub("reports", "x.md"):     "reports/x.md",
		paths.MemorySub("techdebt", "area.md"): "techdebt/area.md",
		nativeIndex():                          "MEMORY.md",
	} {
		if got != filepath.Join(s, want) {
			t.Errorf("got %s, want %s", got, filepath.Join(s, want))
		}
	}
	if got := paths.MemorySubDir("reports"); got != filepath.Join(s, "reports") {
		t.Errorf("paths.MemorySubDir(reports) = %s", got)
	}
}

func TestRequireFlatNewerRefusesOlderNotes(t *testing.T) {
	isolatedConfig(t)
	stamp := filepath.Join(paths.MemoryStoreDir(), layoutFile)
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ stamp, wantErr string }{
		{"flat 9", "store is flat 9, this binary writes flat 4: install a newer claude-code"}, {"flat 4", ""},
	} {
		if err := os.WriteFile(stamp, []byte(tc.stamp+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := stampLayout(); (tc.wantErr == "" && err != nil) || (tc.wantErr != "" && fmt.Sprint(err) != tc.wantErr) {
			t.Errorf("stampLayout on %q = %v, want %q", tc.stamp, err, tc.wantErr)
		}
		if b, _ := os.ReadFile(stamp); string(b) != tc.stamp+"\n" {
			t.Errorf("stamp = %q, want %q untouched", b, tc.stamp)
		}
	}
}

func TestStampLayoutUpgradesOlderStamp(t *testing.T) {
	for v := 1; v < storeVersion; v++ {
		t.Run(strconv.Itoa(v), func(t *testing.T) {
			isolatedConfig(t)
			write(t, Path(layoutFile), fmt.Sprintf("flat %d\n", v))
			const body = "# Notes\n\n## repeated\nbody\n\n## repeated\nundated and id-less\n"
			write(t, Path("decisionLog.md"), body)
			if err := EnsureBank(""); err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(Path(layoutFile)); err != nil || string(b) != layoutFlat+"\n" {
				t.Fatalf("stamp = %q, err=%v", b, err)
			}
			if b, err := os.ReadFile(Path("decisionLog.md")); err != nil || string(b) != body {
				t.Fatalf("existing data changed: %q, err=%v", b, err)
			}
		})
	}
}

func TestStampLayoutRefusesMalformedAndUnreadable(t *testing.T) {
	for _, stamp := range []string{"", " \n", "flat +4", "flat -1", "flat 0", "flat nope", "flat 4 extra", "other 4", "directory"} {
		t.Run(stamp, func(t *testing.T) {
			isolatedConfig(t)
			if stamp == "directory" {
				write(t, filepath.Join(Path(layoutFile), "keep"), "stored data")
			} else {
				write(t, Path(layoutFile), stamp)
			}
			ran := false
			err := withBankLock(BankLockWait, func() error { ran = true; return nil })
			if err == nil || ran {
				t.Fatalf("refusal err=%v, writer ran=%t", err, ran)
			}
			p, want := Path(layoutFile), stamp
			if stamp == "directory" {
				p, want = filepath.Join(p, "keep"), "stored data"
			}
			if b, err := os.ReadFile(p); err != nil || string(b) != want {
				t.Fatalf("refused stamp changed: %q, err=%v", b, err)
			}
		})
	}
}

func TestStampLayoutWaitsForWriterLock(t *testing.T) {
	isolatedConfig(t)
	write(t, Path(layoutFile), "flat 3\n")
	if err := WithBankLock(BankLockWait, func() error {
		err := withBankLock(time.Millisecond, func() error { t.Error("writer ran without lock"); return nil })
		if !errors.Is(err, ErrBankBusy) {
			t.Errorf("busy writer err=%v", err)
		}
		if b, err := os.ReadFile(Path(layoutFile)); err != nil || string(b) != "flat 3\n" {
			t.Errorf("busy writer changed stamp: %q, err=%v", b, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
