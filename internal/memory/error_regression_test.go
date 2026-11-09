package memory

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memstore"
)

func TestAliasRejectsIncompleteProjectLinks(t *testing.T) {
	e := newAliasEnv(t)
	link := seedAliasFold(t, e)
	broken := filepath.Join(e.cfg, "projects", "unreadable")
	if err := os.Symlink("unreadable", broken); err != nil {
		t.Fatal(err)
	}
	before := aliasTreeHash(t, memstore.StoreDir())
	if _, err := alias(t, e, false); err == nil {
		t.Error("alias accepted an incomplete project link scan")
	}
	if got := aliasTreeHash(t, memstore.StoreDir()); got != before {
		t.Error("failed project link scan changed or removed the source store")
	}
	if got, err := os.Readlink(link); err != nil || got != e.db {
		t.Fatalf("source link = %q, %v; want %q unchanged", got, err, e.db)
	}
}

func TestDoctorCachesUnreadableBankWarning(t *testing.T) {
	isolatedConfig(t)
	root := memstore.HomeBankDir()
	if err := os.MkdirAll(filepath.Join(root, "conventions.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	problems := strings.Join(Doctor(root, DefaultStaleDays, now), "\n")
	if !strings.Contains(problems, "conventions.md:") || !strings.Contains(problems, "unreadable") {
		t.Fatalf("doctor silently skipped an unreadable bank file: %q", problems)
	}
	if err := WriteDoctorNotice(root, DefaultStaleDays, now); err != nil {
		t.Fatal(err)
	}
	notice := mustRead(t, DoctorNoticePath(root))
	if !strings.Contains(notice, "conventions.md:") || !strings.Contains(notice, "unreadable") {
		t.Fatalf("cached healthy notice for an unreadable bank: %q", notice)
	}
}

func TestDigestInputLiteralBracketConfig(t *testing.T) {
	isolatedConfig(t)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude[work]"))
	dir := memstore.HomeBankDir()
	write(t, filepath.Join(memstore.ArchiveDir(dir), "2026-01.md"), "first archive fact")
	write(t, filepath.Join(memstore.ArchiveDir(dir), "2026-02.md"), "second archive fact")
	write(t, filepath.Join(memstore.ArchiveDir(dir), DigestFile), "old digest must not be summarized")
	got, used, err := digestInput(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(used, []string{"2026-01.md", "2026-02.md"}) {
		t.Fatalf("archive sources under literal bracket path = %v", used)
	}
	if !strings.Contains(got, "first archive fact") || !strings.Contains(got, "second archive fact") {
		t.Fatalf("digest lost archive contents: %q", got)
	}
	if strings.Contains(got, "old digest") {
		t.Fatalf("digest included itself: %q", got)
	}
}

func TestSessionSurfaceRelativeNativeLink(t *testing.T) {
	isolatedConfig(t)
	initStore(t)
	root := gitRepo(t, t.TempDir(), "relative-link")
	dir, err := memstore.EnsureRepoDir(root)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, bank.NativeIndexFile), "unique native-only index marker\n")
	link := memstore.ProjectsMemory(memstore.ProjectSlug(root))
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Rel(filepath.Dir(link), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(link, bank.NativeIndexFile)); got != "unique native-only index marker\n" {
		t.Fatalf("relative native link is not readable: %q", got)
	}
	got := SessionSurface(root)
	if strings.Contains(got, "unique native-only index marker") || strings.Contains(got, "native link missing or blocked") {
		t.Fatalf("surface duplicated the index despite a valid relative native link: %q", got)
	}
}
