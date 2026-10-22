package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cutover marks the migration complete and creates the store's git repo — the two facts CutOver()
// and StartupGuard key off. Callers must already hold an isolated CLAUDE_CONFIG_DIR (migrateConfig/tempConfig).
func cutover(t *testing.T) {
	t.Helper()
	write(t, filepath.Join(MigrateStateDir(), "20260101-000000.journal.json"), `{"phase":"complete","manifest":"/m/done.json"}`)
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
}

// TestStartupGuardMigratedUnpausedThenPausesWithoutGitRepo: a complete journal with no store git repo yet
// stays paused (Migrated true); StoreRepo().Ensure() is what releases it.
func TestStartupGuardMigratedUnpausedThenPausesWithoutGitRepo(t *testing.T) {
	migrateConfig(t)
	write(t, filepath.Join(MigrateStateDir(), "20260101-000000.journal.json"), `{"phase":"complete","manifest":"/m/done.json"}`)

	g := StartupGuard()
	if !g.Paused || !g.Migrated || !strings.Contains(g.Notice, "no git repo") {
		t.Fatalf("guard before the store exists = %+v, want paused+migrated naming the missing repo", g)
	}

	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	g = StartupGuard()
	if g.Paused || !g.Migrated {
		t.Fatalf("guard after Ensure = %+v, want unpaused and migrated", g)
	}
}

// TestCutoverSwitchesHomeBankDirAndRepoResolution: HomeBankDir/Dir resolve against the legacy tree before a
// completed+stored migration, and against the unified store after.
func TestCutoverSwitchesHomeBankDirAndRepoResolution(t *testing.T) {
	migrateConfig(t)
	repo := gitRepo(t, t.TempDir(), "proj")

	if HomeBankDir() != LegacyGlobalDir() {
		t.Fatalf("pre-cutover HomeBankDir = %s, want legacy %s", HomeBankDir(), LegacyGlobalDir())
	}
	wantLegacy := filepath.Join(LegacyGlobalDir(), "proj")
	if got := Dir(repo); got != wantLegacy {
		t.Fatalf("pre-cutover Dir = %s, want %s", got, wantLegacy)
	}

	cutover(t)

	if want := filepath.Join(StoreDir(), "home"); HomeBankDir() != want {
		t.Errorf("post-cutover HomeBankDir = %s, want %s", HomeBankDir(), want)
	}
	want, err := RepoDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := Dir(repo); got != want {
		t.Errorf("post-cutover Dir = %s, want RepoDir %s", got, want)
	}
	if NativeDir(repo) != Dir(repo) {
		t.Errorf("NativeDir = %s, want Dir %s", NativeDir(repo), Dir(repo))
	}
	gotDirs := NativeDirs(repo)
	wantDirs := []string{HomeBankDir(), Dir(repo)}
	if len(gotDirs) != 2 || gotDirs[0] != wantDirs[0] || gotDirs[1] != wantDirs[1] {
		t.Errorf("NativeDirs = %v, want %v", gotDirs, wantDirs)
	}
}

// TestSettleUnifiedSkipsMissingStoreDirAndCommitsOnce: a repo never scaffolded under the store leaves no
// stray dir behind, and the pass still lands exactly one memory-Git commit.
func TestSettleUnifiedSkipsMissingStoreDirAndCommitsOnce(t *testing.T) {
	migrateConfig(t)
	cutover(t)
	repo := gitRepo(t, t.TempDir(), "proj")

	own := Dir(repo)
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatalf("test setup: %s must not exist yet", own)
	}
	before, err := StoreRepo().Log(50)
	if err != nil {
		t.Fatal(err)
	}

	s, err := settleUnified(repo, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Indexed {
		t.Errorf("settlement = %+v, want Indexed", s)
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Errorf("settleUnified created a stray dir at %s", own)
	}
	after, err := StoreRepo().Log(50)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Errorf("commits before=%d after=%d, want exactly +1", len(before), len(after))
	}
}

// TestSettleAllCoversEveryStore: every repo store plus the store root itself gets its region regenerated
// and the catalog written.
func TestSettleAllCoversEveryStore(t *testing.T) {
	migrateConfig(t)
	cutover(t)
	a := gitRepo(t, t.TempDir(), "alpha")
	b := gitRepo(t, t.TempDir(), "beta")
	da, err := EnsureRepoDir(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := EnsureRepoDir(b)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(da, "activeContext.md"), "# Active\nalpha focus\n")
	write(t, filepath.Join(db, "activeContext.md"), "# Active\nbeta focus\n")

	s, err := SettleAll(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.Indexed {
		t.Fatalf("settlement = %+v, want Indexed", s)
	}
	for _, dir := range []string{da, db, HomeBankDir()} {
		if _, err := os.Stat(filepath.Join(dir, NativeIndexFile)); err != nil {
			t.Errorf("%s missing its regenerated region: %v", dir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(StoreDir(), CatalogFile)); err != nil {
		t.Error("catalog not written")
	}
}
