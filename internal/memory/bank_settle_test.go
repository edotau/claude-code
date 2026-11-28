package memory

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// runBank runs fn on a fresh bank in an isolated flat config.
func runBank(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	isolatedConfig(t)
	if err := ensure(""); err != nil {
		t.Fatal(err)
	}
	fn(t)
}

func TestTolerateIndexKeepsNestedFailures(t *testing.T) {
	commitErr, closeErr := errors.New("commit busy"), errors.New("db close failed")
	advisory := fmt.Errorf("%w: pin left out", errIndexProblem)
	for _, err := range []error{
		errors.Join(errors.Join(commitErr, advisory), closeErr),
		errors.Join(errors.Join(advisory, commitErr)),
		errors.Join(errors.Join(closeErr, advisory)),
	} {
		got := tolerateIndex(err)
		for _, want := range []error{commitErr, closeErr} {
			if errors.Is(got, want) != errors.Is(err, want) {
				t.Errorf("tolerateIndex(%v) = %v; lost %v", err, got, want)
			}
		}
		if errors.Is(got, errIndexProblem) {
			t.Errorf("index advisory was not filtered: %v", got)
		}
	}
	if got := tolerateIndex(errors.Join(errors.Join(advisory))); got != nil {
		t.Errorf("advisory alone returned %v", got)
	}
}

func TestSnapshotRestoreKeepsMode(t *testing.T) {
	file := filepath.Join(t.TempDir(), "catalog.md")
	write(t, file, "before\n")
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := snapshotFile(file)
	if err != nil {
		t.Fatal(err)
	}
	write(t, file, "after\n")
	if err := snap.restore(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o644 || mustRead(t, file) != "before\n" {
		t.Fatalf("restore lost bytes or mode: %v, %v", fi, err)
	}
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
		before := rejoin("# Session history", entries)
		write(t, path, before)
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
		if got, want := mustRead(t, path), before[:strings.Index(before, entries[sessionHistoryMaxEntries])-1]; got != want {
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

func TestRotationPreservesEntryBytes(t *testing.T) {
	runBank(t, func(t *testing.T) {
		var body strings.Builder
		header := "# History\n\n\n"
		body.WriteString(header)
		for i := range sessionHistoryMaxEntries + 1 {
			fmt.Fprintf(&body, "## Session %d\n\nbody %d\n\n\n", i, i)
		}
		in := body.String()
		last := strings.LastIndex(in, "## Session ")
		write(t, Path("sessionHistory.md"), in)
		if n, err := rotateSessionHistory(testEnv().now); err != nil || n != 1 {
			t.Fatalf("rotate = %d, %v", n, err)
		}
		// the newline joining the moved entry goes with it, so the file is what the store renders once that row moves
		if got := mustRead(t, Path("sessionHistory.md")); got != in[:last-1] {
			t.Errorf("retained entries changed: %q", got)
		}
		archive := archivePath("sessionHistory.md", testEnv().now)
		if got := mustRead(t, archive); !strings.HasSuffix(got, in[last:]) {
			t.Errorf("archived entry changed: %q", got)
		}
	})
}

func TestArchiveAppendsRefuseOpenFence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.md")
	before := "# Archive\n\n```\nexample\n"
	write(t, path, before)
	if err := appendBlocks(path, []string{"## New <!-- id:12345678 -->"}); err == nil {
		t.Error("prune append hid an entry inside a fence")
	}
	if _, err := appendUnarchived(path, []string{"## New <!-- id:12345678 -->"}); err == nil {
		t.Error("rotation append hid an entry inside a fence")
	}
	if got := mustRead(t, path); got != before {
		t.Errorf("refused append changed bytes: %q", got)
	}
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

// A failed rotation leaves the blocks settle stamped wrapped and every file it did not change byte-identical.
func TestSettleDirWrapsOnlyChangedBlocksWhenALaterStepFails(t *testing.T) {
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
		files := []string{Path("decisionLog.md"), Path("sessionHistory.md")} // every entry got its id
		if changed, err := formatFiles(files, true); err != nil || len(changed) != 0 {
			t.Fatalf("settle left unwrapped files %v (err %v)", changed, err)
		}
		if got := mustRead(t, Path("activeContext.md")); got != "# Active\n\n"+long {
			t.Errorf("settle rewrote a file it did not change:\n%s", got)
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
		if dirty, err := StoreRepo().status(name); err != nil || len(dirty) != 0 {
			t.Errorf("%s remains dirty after settle: %v, %v", name, dirty, err)
		}
	}
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
		write(t, Path("activeContext.md"), "Hook line.\n") // keeps the router's "last" date independent of index.md
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

// dbTruthBank is an initialized bank over a fake export db wired as OpenExportDB.
func dbTruthBank(t *testing.T) *fakeExportDB {
	t.Helper()
	isolatedConfig(t)
	initStore(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	db := &fakeExportDB{root: paths.MemoryStoreDir(), docs: map[string]string{}, last: map[string]string{}}
	oldOpen := OpenExportDB
	OpenExportDB = func() (ExportDB, func() error, error) { return db, func() error { return nil }, nil }
	t.Cleanup(func() { OpenExportDB = oldOpen })
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
	if dirty, err := StoreRepo().status(); err != nil || len(dirty) != 0 {
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
		kept := filepath.Join(HeldDir(), "conventions.md.db-conflict-"+textutil.SHA256Hex([]byte("settled\n"))[:12])
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

// H-6: settle imports the Markdown a stored files truth left behind, then retires the truth to db.
func TestSettleDBRetiresFilesTruthAfterImport(t *testing.T) {
	db := dbTruthBank(t)
	write(t, truthPath(), "files\n")
	write(t, Path("conventions.md"), "# Conventions\n\n- written under files truth\n")
	if _, err := settleBankWith(time.Now(), nil, "settle: test"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(db.docs["conventions.md"], "- written under files truth") {
		t.Fatalf("the files-era edit was not imported: %q", db.docs["conventions.md"])
	}
	if got := strings.TrimSpace(mustRead(t, truthPath())); got != "db" {
		t.Fatalf("truth = %q after settle, want db", got)
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
	if rotated, err := settleBankWith(now, nil, "settle: test"); err != nil || rotated != 2 { // the merged archive takes them
		t.Fatalf("settle = %d, %v; want 2 rotated, nil", rotated, err)
	}
	for _, body := range []string{"db side body", "disk side body", "prior archived body", "Distinct body 31"} {
		if !strings.Contains(db.docs[archive], body) || !strings.Contains(mustRead(t, Path(archive)), body) {
			t.Fatalf("archive lost %q: db %q", body, db.docs[archive])
		}
	}
	if len(db.conflicts) != 0 {
		t.Fatalf("merged archive recorded conflicts %v", db.conflicts)
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

// A settle whose guard keeps a conflict apart leaves the files truth in place: nothing was imported for that doc.
func TestSettleDBKeepsFilesTruthOnConflict(t *testing.T) {
	dbTruthBank(t)
	write(t, Path("conventions.md"), "# Conventions\n\n- old\n")
	if _, err := settleBankWith(time.Now(), nil, "settle: test"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(Path("conventions.md")); err != nil { // deleted outside after an export: a kept conflict
		t.Fatal(err)
	}
	write(t, truthPath(), "files\n")
	if _, err := settleBankWith(time.Now(), nil, "settle: test"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(mustRead(t, truthPath())); got != "files" {
		t.Fatalf("truth = %q after a conflicting guard, want files kept", got)
	}
}

// An export db that cannot open: a files truth settles the files alone and stays; a db truth keeps the error.
func TestSettleDBExportOpenFailure(t *testing.T) {
	dbTruthBank(t)
	OpenExportDB = func() (ExportDB, func() error, error) { return nil, nil, errors.New("export db down") }
	if _, err := settleBankWith(time.Now(), nil, "settle: test"); err == nil {
		t.Fatal("a db truth settled with its export db down")
	}
	write(t, truthPath(), "files\n")
	if _, err := settleBankWith(time.Now(), nil, "settle: test"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(mustRead(t, truthPath())); got != "files" {
		t.Fatalf("truth %q after settling with no export db, want files kept", got)
	}
}

func TestSettleMovesBankCopyToHeldDir(t *testing.T) {
	dbTruthBank(t)
	old := Path("conventions.md.db-conflict-abcdef012345")
	write(t, old, "kept by an older build\n")
	if _, err := settleBankWith(time.Now(), nil, "settle: test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("bank copy still in the bank: %v", err)
	}
	if got := mustRead(t, filepath.Join(HeldDir(), "conventions.md.db-conflict-abcdef012345")); got != "kept by an older build\n" {
		t.Fatalf("held copy = %q", got)
	}
}

func TestPruneHeldDropsOldAndPastTwenty(t *testing.T) {
	isolatedConfig(t)
	now := time.Now()
	put := func(name string, age time.Duration) string {
		p := filepath.Join(HeldDir(), name)
		write(t, p, "x\n")
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	var conv []string
	for i := range heldMaxPerFile + 1 {
		conv = append(conv, put(fmt.Sprintf("conventions.md.db-conflict-%012x", i), time.Duration(i)*time.Minute))
	}
	fresh, stale := put("decisionLog.md.db-conflict-000000000001", time.Hour), put("decisionLog.md.db-conflict-000000000002", 8*24*time.Hour)
	stamped := put("decisionLog.md.export-old-000000000003-"+strconv.FormatInt(now.Add(-9*24*time.Hour).UnixNano(), 36)+"0", 0)
	if err := pruneHeld(now); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{conv[0]: true, conv[heldMaxPerFile-1]: true, conv[heldMaxPerFile]: false, fresh: true, stale: false, stamped: false} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}

// indexClean runs the index over the test bank and fails on any error.
func indexClean(t *testing.T) {
	t.Helper()
	if err := Index(&bytes.Buffer{}, &bytes.Buffer{}, IndexOptions{}); err != nil {
		t.Fatalf("index: %v", err)
	}
}

// A store with a leftover projectContext.md settles and indexes clean and never recreates it.
func TestLeftoverProjectContextSettlesAndIndexes(t *testing.T) {
	runBank(t, func(t *testing.T) {
		left := Path("projectContext.md")
		write(t, left, "# Project Context\n\nOld orientation.\n")
		indexClean(t)
		indexClean(t)
		if slices.Contains(BankFiles(), "projectContext.md") {
			t.Fatal("projectContext.md is still a bank file")
		}
		for _, f := range []string{routerFile, nativeIndexFile} {
			if b, _ := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), f)); strings.Contains(string(b), "→ projectContext.md") {
				t.Errorf("%s still points at projectContext.md:\n%s", f, b)
			}
		}
		if err := os.Remove(left); err != nil && !os.IsNotExist(err) { // settle may fold it away as a loose note
			t.Fatal(err)
		}
		if _, err := Init(); err != nil {
			t.Fatal(err)
		}
		indexClean(t)
		if _, err := os.Stat(left); !os.IsNotExist(err) {
			t.Errorf("projectContext.md was recreated (stat err=%v)", err)
		}
	})
}
