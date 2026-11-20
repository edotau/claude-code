package memory

import (
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

// fakeCredential builds a credential-shaped string at runtime; it is not a real secret.
func fakeCredential() string { return "sk-" + "ant-api03-" + strings.Repeat("x9Y_", 6) }

// TestSettleBankRefusesCredentialFile: a credential-shaped file in the store is refused without leaking it.
func TestSettleBankRefusesCredentialFile(t *testing.T) {
	tempConfig(t)
	if _, err := Init(); err != nil {
		t.Fatal(err)
	}
	write(t, Path("leak.md"), "key: "+fakeCredential()+"\n")
	_, err := settleBank(time.Now())
	if err == nil || !strings.Contains(err.Error(), "credential-shaped content") {
		t.Fatalf("a credential-shaped file in the store must be refused, got %v", err)
	}
	if strings.Contains(err.Error(), fakeCredential()) {
		t.Fatal("the refusal leaked the credential")
	}
}

// TestSettleDirWrapsTypedFilesWhenALaterStepFails: a failed rotation still leaves every typed file wrapped.
func TestSettleDirWrapsTypedFilesWhenALaterStepFails(t *testing.T) {
	runBank(t, func(t *testing.T) {
		long := "- " + strings.Repeat("word ", 40) + "\n"
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
		writeNote(t, "notes", "long.md", "---\nname: Long\ndescription: "+strings.Repeat("é", 200)+"\n---\n")
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
			"- [bad](notes/bad.md) — bad", "- [Long](notes/long.md) — " + strings.Repeat("é", 159) + "…",
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
			writeNote(t, "notes", fmt.Sprintf("n%03d.md", i), "---\nname: N\ndescription: "+strings.Repeat("x", 150)+"\n---\n")
		}
		small := len(catalogOf(t))
		if after := len(sessionSurface(t.TempDir())); after > before+len("<!-- catalog: ")+len(paths.MemoryStoreDir())+200 || small < 20000 {
			t.Errorf("surface grew %d -> %d (catalog %d bytes)", before, after, small)
		}
	})
}
