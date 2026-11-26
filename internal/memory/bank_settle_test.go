package memory

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// runBank runs fn on a fresh bank in an isolated flat config.
func runBank(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	isolatedConfig(t)
	if err := Ensure(""); err != nil {
		t.Fatal(err)
	}
	fn(t)
}

func TestRotateSessionHistoryRetryDoesNotDuplicateArchive(t *testing.T) {
	runBank(t, func(t *testing.T) {
		date := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
		entries := make([]string, sessionHistoryMaxEntries+2)
		for i := range entries {
			entries[i] = fmt.Sprintf("## Session %02d\n\nDistinct body %02d.", i, i)
		}
		path := Path("sessionHistory.md")
		archive := paths.MemorySub("archive", "sessionHistory-2026-11.md")
		write(t, path, rejoin("# Session history", entries))
		// Recreate an archive write that persisted before the source rewrite failed, plus one still-unarchived entry.
		write(t, archive, rejoin("# Archived sessions", entries[sessionHistoryMaxEntries:sessionHistoryMaxEntries+1]))

		n, err := rotateSessionHistory(date)
		if err != nil || n != 2 {
			t.Fatalf("retry rotation = %d, %v; want 2, nil", n, err)
		}
		_, archived := splitHeadingBlocks(mustRead(t, archive))
		if want := entries[sessionHistoryMaxEntries:]; !slices.Equal(archived, want) {
			t.Fatalf("retry duplicated or lost archived entries:\n got: %q\nwant: %q", archived, want)
		}
		if got, want := mustRead(t, path), rejoin("# Session history", entries[:sessionHistoryMaxEntries]); got != want {
			t.Fatalf("retained history:\n got: %q\nwant: %q", got, want)
		}
	})
}

func TestRotateSessionHistoryReportsReadError(t *testing.T) {
	runBank(t, func(t *testing.T) {
		if n, err := rotateSessionHistory(time.Now()); n != 0 || err != nil {
			t.Fatalf("missing history: got %d, %v", n, err)
		}
		path := Path("sessionHistory.md")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		_, readErr := os.ReadFile(path)
		var want *os.PathError
		if !errors.As(readErr, &want) {
			t.Fatalf("fixture did not produce a read error: %v", readErr)
		}
		n, err := rotateSessionHistory(time.Now())
		if n != 0 || !errors.Is(err, want.Err) {
			t.Fatalf("unreadable history: got %d, %v; want wrapped %v", n, err, want.Err)
		}
	})
}

// TestSettleBankLockLivesUnderStateNotStore: settleBank locks BankLockPath() under state, not a dotfile in the store.
func TestSettleBankLockLivesUnderStateNotStore(t *testing.T) {
	cfg := tempConfig(t)
	dir := paths.MemoryStoreDir()
	if _, err := settleBank(time.Now()); err != nil {
		t.Fatal(err)
	}
	want := BankLockPath()
	if !strings.HasPrefix(want, filepath.Join(cfg, "state")) {
		t.Fatalf("bank lock %s is not under state", want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("lock file missing at %s: %v", want, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".postupdate.lock")); err == nil {
		t.Error("a legacy-shaped lock file was written beside the store")
	}
	if _, err := os.Stat(filepath.Join(dir, ".postupdate")); err == nil {
		t.Error("a legacy-shaped lock file was written beside the store")
	}
}

// TestSettleBankRefusesCredentialFile: a credential-shaped file in the store is refused without leaking it.
func TestSettleBankRefusesCredentialFile(t *testing.T) {
	placeholder := useCredentialPlaceholder(t)
	tempConfig(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	write(t, Path("leak.md"), "key: "+placeholder+"\n")
	_, err := settleBank(time.Now())
	if err == nil || !strings.Contains(err.Error(), "credential-shaped content") {
		t.Fatalf("a credential-shaped file in the store must be refused, got %v", err)
	}
	if strings.Contains(err.Error(), placeholder) {
		t.Fatal("the refusal leaked the credential")
	}
}

// TestSettleDirWrapsTypedFilesWhenALaterStepFails: a failed rotation still leaves every typed file wrapped.
func TestSettleDirWrapsTypedFilesWhenALaterStepFails(t *testing.T) {
	runBank(t, func(t *testing.T) {
		long := "- " + string(bytes.Repeat([]byte("word "), 40)) + "\n"
		write(t, Path("activeContext.md"), "# Active\n\n"+long)
		write(t, Path("decisionLog.md"), "# Log\n\n## 2026-11-13 — d\n\n"+long)
		entries := make([]string, sessionHistoryMaxEntries+1)
		for i := range entries {
			entries[i] = fmt.Sprintf("## Session %02d\n\n%s", i, long)
		}
		write(t, Path("sessionHistory.md"), rejoin("# Session history", entries))
		now := time.Date(2026, time.November, 13, 12, 0, 0, 0, time.UTC)
		if err := os.MkdirAll(paths.MemorySub("archive", "sessionHistory-2026-11.md"), 0o700); err != nil { // the rotation cannot append
			t.Fatal(err)
		}

		if _, err := settleDir(now); err == nil {
			t.Fatal("settle succeeded with an unwritable archive")
		}
		files := []string{Path("activeContext.md"), Path("decisionLog.md"), Path("sessionHistory.md")}
		if changed, err := formatFiles(files, true); err != nil || len(changed) != 0 {
			t.Fatalf("settle left unwrapped files %v (err %v)", changed, err)
		}
	})
}

func TestSettleBankCommitsEveryStoreFile(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(paths.MemoryStoreDir(), "topic.md"), "a topic\n")
	write(t, paths.MemorySub("reports", "r.md"), "a report\n")
	write(t, Path("decisionLog.md"), "# Log\n\n## 2026-11-16 — new entry\nBody.\n")
	if _, err := settleBank(time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(aliasRead(t, Path("decisionLog.md")), "<!-- id:") {
		t.Fatal("entry not indexed")
	}
	for _, name := range []string{"decisionLog.md", "topic.md", "reports--r.md", layoutFile} {
		if dirty, err := StoreRepo().Status(name); err != nil || len(dirty) != 0 {
			t.Errorf("%s remains dirty after settle: %v, %v", name, dirty, err)
		}
	}
}

// shadowStep is one settle's stubbed import result.
type shadowStep struct {
	docs    []string
	changed bool
	err     error
	panic   bool
}

func TestSettleShadowLog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		steps  []shadowStep
		lines  []string // docs of each log line, in order, "-" for a cleared set
		notice bool
	}{
		{name: "diverged", steps: []shadowStep{{docs: []string{"a.md"}, changed: true}}, lines: []string{"a.md"}},
		{name: "matched", steps: []shadowStep{{}}},
		{name: "import error", steps: []shadowStep{{err: errors.New("shadow unavailable")}}, notice: true},
		{name: "partial result kept", steps: []shadowStep{{docs: []string{"a.md"}, changed: true, err: errors.New("late")}},
			lines: []string{"a.md"}, notice: true},
		{name: "error keeps the set", steps: []shadowStep{{docs: []string{"a.md"}, changed: true}, {err: errors.New("late")}},
			lines: []string{"a.md"}, notice: true},
		{name: "panic", steps: []shadowStep{{panic: true}}, notice: true},
		{name: "change only", steps: []shadowStep{{docs: []string{"a.md", "b.md"}, changed: true}, {docs: []string{"a.md", "b.md"}},
			{docs: []string{"a.md"}, changed: true}, {changed: true}, {}}, lines: []string{"a.md b.md", "a.md", "-"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runBank(t, func(t *testing.T) {
				var got []string
				for _, st := range tc.steps {
					stubShadowImport(t, st)
					got = append(got, stderrOf(t, func() {
						if _, err := settleBank(time.Now()); err != nil {
							t.Fatal(err)
						}
					}))
				}
				if has := strings.Contains(strings.Join(got, ""), "notice: memory shadow: "); has != tc.notice {
					t.Fatalf("shadow notice = %q, want notice %v", got, tc.notice)
				}
				if lines := shadowLogDocs(t); !slices.Equal(lines, tc.lines) {
					t.Fatalf("shadow log lines = %q, want %q", lines, tc.lines)
				}
			})
		})
	}
}

func TestSettleShadowLogWriteFails(t *testing.T) {
	runBank(t, func(t *testing.T) {
		if err := os.MkdirAll(shadowLogPath(), 0o700); err != nil { // a directory where the log file goes
			t.Fatal(err)
		}
		stubShadowImport(t, shadowStep{docs: []string{"a.md"}, changed: true})
		out := stderrOf(t, func() {
			if _, err := settleBank(time.Now()); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, "notice: memory shadow: ") {
			t.Fatalf("no notice for a failed log write: %q", out)
		}
	})
}

func stubShadowImport(t *testing.T, st shadowStep) {
	t.Helper()
	old := ShadowImport
	t.Cleanup(func() { ShadowImport = old })
	ShadowImport = func() ([]string, bool, error) {
		if st.panic {
			panic("boom")
		}
		return st.docs, st.changed, st.err
	}
}

// shadowLogDocs is each log line's docs joined by space, "-" for an empty set; nil when there is no log.
func shadowLogDocs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(shadowLogPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		var line struct {
			Time string   `json:"time"`
			Docs []string `json:"docs"`
		}
		if err := json.Unmarshal([]byte(l), &line); err != nil || line.Time == "" || line.Docs == nil {
			t.Fatalf("bad shadow log line %q: %v", l, err)
		}
		out = append(out, cmp.Or(strings.Join(line.Docs, " "), "-"))
	}
	return out
}

// stderrOf runs fn and returns what it wrote to os.Stderr, restored by t.Cleanup; never use it in a parallel test.
func stderrOf(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	t.Cleanup(func() { os.Stderr = orig; _ = f.Close() })
	os.Stderr = f
	fn()
	os.Stderr = orig
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeNote writes a native note under the store's folder.
func writeNote(t *testing.T, folder, file, content string) {
	t.Helper()
	write(t, paths.MemorySub(folder, file), content)
}

func catalogOf(t *testing.T) string {
	t.Helper()
	if err := writeCatalog(time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), routerFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCatalogListsEveryNativeNoteOnceSortedWithFallbacks(t *testing.T) {
	runBank(t, func(t *testing.T) {
		writeNote(t, "notes", "b.md", "---\nname: Bee\ndescription: bee desc\n---\nbody\n")
		writeNote(t, "notes", "a.md", "---\nname: Ay\ndescription: >\n  folded one\n  folded two\n---\n")
		writeNote(t, "feedback", "z.md", "---\nname: Zed\n---\nbody\n")
		writeNote(t, "notes", "long.md", "---\nname: Long\ndescription: "+string(bytes.Repeat([]byte("é"), 200))+"\n---\n")
		writeNote(t, "notes", "bad.md", "no front matter\n")
		write(t, Path("projectContext.md"), "Hook line.\n") // keeps the router's "last" date independent of index.md
		router, err := BuildCatalog(time.Unix(0, 0))
		c := catalogOf(t)
		if err != nil || !strings.HasPrefix(c, router) {
			t.Fatalf("router part changed (err=%v)", err)
		}
		want := []string{
			"### feedback/", "- [Zed](feedback/z.md) — z",
			"### notes/", "- [Ay](notes/a.md) — folded one folded two", "- [Bee](notes/b.md) — bee desc",
			"- [bad](notes/bad.md) — bad", "- [Long](notes/long.md) — " + string(bytes.Repeat([]byte("é"), 159)) + "…",
			"1 note(s) listed by file name",
		}
		at := strings.Index(c, "## Native memories")
		for _, w := range want {
			i := strings.Index(c[at:], w)
			if i < 0 {
				t.Fatalf("missing %q in:\n%s", w, c[at:])
			}
			at += i
		}
		if n := strings.Count(c, "](notes/a.md)"); n != 1 {
			t.Errorf("a.md listed %d times", n)
		}
		if c2 := catalogOf(t); c2 != c {
			t.Errorf("second generation differs")
		}
	})
}

func TestTechdebtNoteIsCatalogedAndSearchableButNotPromptRecalled(t *testing.T) {
	runBank(t, func(t *testing.T) {
		writeNote(t, "techdebt", "area.md", "---\nname: Area\ndescription: zorblax debt\n---\nthe zorblax parser is slow\n")
		if c, err := BuildCatalog(time.Unix(0, 0)); err != nil || !strings.Contains(c+catalogOf(t), "](techdebt/area.md)") {
			t.Errorf("techdebt note not cataloged (err=%v)", err)
		}
		hits := SearchBank("zorblax parser", 5)
		if len(hits) != 1 || hits[0].Bank != techdebtDir {
			t.Fatalf("search hits %+v", hits)
		}
		if cands, _ := recallTier(hits, false, 1); len(cands) != 0 {
			t.Errorf("prompt recall admitted techdebt: %+v", cands)
		}
	})
}

func TestCatalogGrowthDoesNotGrowSessionSurface(t *testing.T) {
	runBank(t, func(t *testing.T) {
		before := len(sessionSurface(t.TempDir()))
		for i := range 150 {
			writeNote(t, "notes", fmt.Sprintf("n%03d.md", i), "---\nname: N\ndescription: "+string(bytes.Repeat([]byte("!"), 150))+"\n---\n")
		}
		small := len(catalogOf(t))
		if after := len(sessionSurface(t.TempDir())); after > before+len("<!-- catalog: ")+len(paths.MemoryStoreDir())+200 || small < 20000 {
			t.Errorf("surface grew %d -> %d (catalog %d bytes)", before, after, small)
		}
	})
}

// dbTruthBank is an initialized bank in db truth over a fake export db wired as OpenExportDB.
func dbTruthBank(t *testing.T) *fakeExportDB {
	t.Helper()
	isolatedConfig(t)
	initStore(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	db := &fakeExportDB{root: paths.MemoryStoreDir(), docs: map[string]string{}, last: map[string]string{}}
	oldOpen, oldFlip := OpenExportDB, ExportBeforeFlip
	OpenExportDB = func() (ExportDB, func() error, error) { return db, func() error { return nil }, nil }
	t.Cleanup(func() { OpenExportDB, ExportBeforeFlip = oldOpen, oldFlip })
	if err := SetTruth("db"); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSettleDBImportsRotatesAndExportsByteIdentical(t *testing.T) {
	db := dbTruthBank(t)
	now := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	entries := make([]string, sessionHistoryMaxEntries+2)
	for i := range entries {
		entries[i] = fmt.Sprintf("## Session %02d\n\nDistinct body %02d.", i, i)
	}
	write(t, Path("sessionHistory.md"), rejoin("# Session history", entries))
	write(t, Path("conventions.md"), "# Conventions\n\n- outside edit\n")
	rotated, err := settleBankWith(now, nil, "settle: test")
	if err != nil || rotated != 2 {
		t.Fatalf("db settle = %d, %v; want 2, nil", rotated, err)
	}
	if !strings.Contains(db.docs["conventions.md"], "- outside edit") {
		t.Fatalf("outside edit not imported into the db: %q", db.docs["conventions.md"])
	}
	archive := "archive/sessionHistory-2026-11.md"
	if _, held := splitHeadingBlocks(db.docs[archive]); len(held) != 2 {
		t.Fatalf("db archive holds %d entries, want the 2 rotated: %q", len(held), db.docs[archive])
	}
	if _, kept := splitHeadingBlocks(db.docs["sessionHistory.md"]); len(kept) != sessionHistoryMaxEntries {
		t.Fatalf("db sessionHistory keeps %d entries, want %d", len(kept), sessionHistoryMaxEntries)
	}
	wantExportMatchesDB(t, db)
	if dirty, err := StoreRepo().Status(); err != nil || len(dirty) != 0 {
		t.Errorf("export left dirty after settle: %v, %v", dirty, err)
	}
	if rep := exportGuard(t, db); len(rep.Written)+len(rep.Imported)+len(rep.Conflicts) != 0 {
		t.Errorf("guard after settle = %+v, want nothing to do", rep)
	}
}

func wantExportMatchesDB(t *testing.T, db *fakeExportDB) {
	t.Helper()
	for rel, want := range db.docs {
		if got := mustRead(t, filepath.Join(db.root, filepath.FromSlash(rel))); got != want {
			t.Errorf("%s export differs from the db:\n got: %q\nwant: %q", rel, got, want)
		}
	}
}

func TestSetTruthFilesExportsDBFirst(t *testing.T) {
	db := dbTruthBank(t)
	write(t, Path("conventions.md"), "# Conventions\n\n- old\n")
	if _, err := settleBankWith(time.Now(), nil, "settle: test"); err != nil {
		t.Fatal(err)
	}
	db.docs["conventions.md"] = "# Conventions\n\n- db only\n"
	ExportBeforeFlip = nil
	if err := SetTruth("files"); err == nil || Truth() != "db" {
		t.Fatalf("flip with no export = %v, truth %q; want a refusal and db", err, Truth())
	}
	ExportBeforeFlip = func() error { _, err := ExportGuardLocked(db, db.root); return err }
	if err := SetTruth("files"); err != nil || Truth() != "files" {
		t.Fatalf("flip = %v, truth %q; want nil and files", err, Truth())
	}
	if got := mustRead(t, Path("conventions.md")); got != "# Conventions\n\n- db only\n" {
		t.Fatalf("file after flip = %q, want the db-only change exported first", got)
	}
}

func TestWriteIfStillKeepsOutsideEdit(t *testing.T) {
	runBank(t, func(t *testing.T) {
		path := Path("conventions.md")
		write(t, path, "# Conventions\n\n- seen\n")
		write(t, path, "# Conventions\n\n- outside\n") // an editor saves after settle read the file
		ok, err := writeIfStill(path, []byte("# Conventions\n\n- seen\n"), true, []byte("settled\n"), 0o600)
		if ok || err != nil {
			t.Fatalf("stale write = %v, %v; want skipped, nil", ok, err)
		}
		if got := mustRead(t, path); got != "# Conventions\n\n- outside\n" {
			t.Fatalf("outside edit lost: %q", got)
		}
		kept := path + ".db-conflict-" + sha256Hex([]byte("settled\n"))[:12]
		if got := mustRead(t, kept); got != "settled\n" {
			t.Fatalf("kept copy = %q, want the settle result", got)
		}
		if _, isCopy := KeptCopyBase(filepath.Base(kept)); !isCopy {
			t.Fatalf("%s is not a listed kept copy", kept)
		}
		write(t, path, "# Conventions\n\n- seen\n")
		if ok, err := writeIfStill(path, []byte("# Conventions\n\n- seen\n"), true, []byte("settled\n"), 0o600); !ok || err != nil {
			t.Fatalf("fresh write = %v, %v; want written", ok, err)
		}
		if got := mustRead(t, path); got != "settled\n" {
			t.Fatalf("file = %q, want the settle result", got)
		}
	})
}

func TestSettleKeepsEditMadeAfterRead(t *testing.T) {
	runBank(t, func(t *testing.T) {
		path := Path("conventions.md")
		write(t, path, "# Conventions\n\n- one\n- two\n")
		data, _ := os.ReadFile(path)
		write(t, path, "# Conventions\n\n- one\n- two\n- outside\n")
		d := newBankDoc(path, string(data))
		d.setBlock(0, "- one!")
		if err := writeDirtyDocs([]*bankDoc{d}); err != nil {
			t.Fatal(err)
		}
		if got := mustRead(t, path); !strings.Contains(got, "- outside") {
			t.Fatalf("revision overwrote the outside edit: %q", got)
		}
	})
}

func TestSetTruthFilesRefreshesCatalog(t *testing.T) {
	db := dbTruthBank(t)
	write(t, Path("conventions.md"), "# Conventions\n\n- old\n")
	if err := os.WriteFile(filepath.Join(paths.MemoryStoreDir(), routerFile), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ExportBeforeFlip = func() error { _, err := ExportGuardLocked(db, db.root); return err }
	if err := SetTruth("files"); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(paths.MemoryStoreDir(), routerFile)); got == "stale\n" {
		t.Fatal("catalog.md stayed stale after the flip to files")
	}
}

// overflowHistory is a sessionHistory.md body holding two entries past the cap.
func overflowHistory() string {
	entries := make([]string, sessionHistoryMaxEntries+2)
	for i := range entries {
		entries[i] = fmt.Sprintf("## Session %02d\n\nDistinct body %02d.", i, i)
	}
	return rejoin("# Session history", entries)
}

// settledArchive settles once with an archive file on disk so the db holds it and has rendered it.
func settledArchive(t *testing.T, db *fakeExportDB, now time.Time) string {
	t.Helper()
	archive := "archive/sessionHistory-" + now.Format("2006-01") + ".md"
	write(t, Path(archive), "# Archive\n\n## Old entry\n\nprior archived body.\n")
	if _, err := settleBankWith(now, nil, "settle: seed"); err != nil || !strings.Contains(db.docs[archive], "prior archived body") {
		t.Fatalf("seed settle = %v, db archive %q", err, db.docs[archive])
	}
	return archive
}

func TestSettleDBKeepsArchiveWhenItWasDeletedOutside(t *testing.T) {
	db := dbTruthBank(t)
	now := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	archive := settledArchive(t, db, now)
	if err := os.Remove(Path(archive)); err != nil {
		t.Fatal(err)
	}
	write(t, Path("sessionHistory.md"), overflowHistory())
	if rotated, err := settleBankWith(now, nil, "settle: test"); err != nil || rotated != 0 {
		t.Fatalf("settle = %d, %v; want 0 rotated, nil", rotated, err)
	}
	if !strings.Contains(db.docs[archive], "prior archived body") {
		t.Fatalf("db archive lost its prior rows: %q", db.docs[archive])
	}
	if _, kept := splitHeadingBlocks(db.docs["sessionHistory.md"]); len(kept) != sessionHistoryMaxEntries+2 {
		t.Fatalf("db sessionHistory holds %d entries, want all %d", len(kept), sessionHistoryMaxEntries+2)
	}
}

func TestSettleDBKeepsEntriesWhenArchiveChangedOnBothSides(t *testing.T) {
	db := dbTruthBank(t)
	now := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	archive := settledArchive(t, db, now)
	db.docs[archive] += "\n## Db entry\n\ndb side body.\n"
	write(t, Path(archive), "# Archive\n\n## Old entry\n\nprior archived body.\n\n## Disk entry\n\ndisk side body.\n")
	write(t, Path("sessionHistory.md"), overflowHistory())
	if rotated, err := settleBankWith(now, nil, "settle: test"); err != nil || rotated != 0 {
		t.Fatalf("settle = %d, %v; want 0 rotated, nil", rotated, err)
	}
	if _, kept := splitHeadingBlocks(db.docs["sessionHistory.md"]); len(kept) != sessionHistoryMaxEntries+2 {
		t.Fatalf("db sessionHistory holds %d entries, want all %d (the rotation must not truncate it)", len(kept), sessionHistoryMaxEntries+2)
	}
	if !strings.Contains(db.docs[archive], "db side body") {
		t.Fatalf("db archive lost its side: %q", db.docs[archive])
	}
}

func TestSettleDBSweepSkipsNoteDeletedOutside(t *testing.T) {
	db := dbTruthBank(t)
	now := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	write(t, Path("notes/n.md"), "# N\n\ndb row body.\n")
	if _, err := settleBankWith(now, nil, "settle: seed"); err != nil || !strings.Contains(db.docs["notes/n.md"], "db row body") {
		t.Fatalf("seed settle = %v, db %q", err, db.docs["notes/n.md"])
	}
	if err := os.Remove(Path("notes/n.md")); err != nil {
		t.Fatal(err)
	}
	write(t, Path("n.md"), "# N\n\nstray top-level body.\n")
	if _, err := settleBankWith(now, nil, "settle: test"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(db.docs["notes/n.md"], "db row body") {
		t.Fatalf("db note overwritten by the swept stray: %q", db.docs["notes/n.md"])
	}
	if _, err := os.Stat(Path("n.md")); err != nil {
		t.Fatalf("stray n.md was moved into a conflicted target: %v", err)
	}
}

func TestSettleDBStillRotatesWhenAnotherPathConflicts(t *testing.T) {
	db := dbTruthBank(t)
	now := time.Date(2026, time.November, 8, 12, 0, 0, 0, time.UTC)
	write(t, Path("conventions.md"), "# Conventions\n\n- a\n")
	if _, err := settleBankWith(now, nil, "settle: seed"); err != nil {
		t.Fatal(err)
	}
	db.docs["conventions.md"] += "\n- db edit\n"
	write(t, Path("conventions.md"), "# Conventions\n\n- a\n- disk edit\n")
	write(t, Path("sessionHistory.md"), overflowHistory())
	if rotated, err := settleBankWith(now, nil, "settle: test"); err != nil || rotated != 2 {
		t.Fatalf("settle = %d, %v; want 2 rotated, nil", rotated, err)
	}
}
