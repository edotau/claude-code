package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func forceFixture(t *testing.T) (repo *Repo, main, dest string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	main = gitRepo(t)
	repo, err := ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err = Create(repo, "force", "")
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, dest, "commit", "--allow-empty", "-q", "-m", "unmerged")
	if err := os.WriteFile(filepath.Join(dest, "scratch.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo, main, dest
}

func TestForceCleanRefusesDirtyUnmergedWithoutForce(t *testing.T) {
	repo, _, dest := forceFixture(t)
	res, err := Clean(repo, "force", false)
	if err != nil || res.Removed {
		t.Fatalf("plain Clean must refuse: removed=%v err=%v", res.Removed, err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("worktree must remain")
	}
}

func TestForceCleanDiscardsDirtyUnmergedAndReports(t *testing.T) {
	repo, main, dest := forceFixture(t)
	var got []DiscardSummary
	res, err := CleanForce(repo, "force", false, func(s DiscardSummary) { got = append(got, s) })
	if err != nil || !res.Removed || !res.BranchDeleted {
		t.Fatalf("want removed+branch deleted: %+v err=%v", res, err)
	}
	if len(got) != 1 || got[0].Dirty != 1 || got[0].Unmerged != 1 || got[0].Branch != "worktree-force" {
		t.Fatalf("summary = %+v", got)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("dir still exists")
	}
	if branchExists(main, "worktree-force") {
		t.Fatal("branch still exists")
	}
}

func TestForceCleanDryRunReportsButKeeps(t *testing.T) {
	repo, main, dest := forceFixture(t)
	called := 0
	res, err := CleanForce(repo, "force", true, func(DiscardSummary) { called++ })
	if err != nil || !res.Removed || called != 1 {
		t.Fatalf("dry-run: %+v err=%v called=%d", res, err, called)
	}
	if _, err := os.Stat(dest); err != nil || !branchExists(main, "worktree-force") {
		t.Fatal("dry-run must not remove anything")
	}
}

func TestForceCleanRefusesInUse(t *testing.T) {
	repo, main, dest := forceFixture(t)
	stubProcesses(t, func(string) ([]Proc, error) { return []Proc{{PID: 999999, Comm: "fakeproc"}}, nil })
	called := false
	res, err := CleanForce(repo, "force", false, func(DiscardSummary) { called = true })
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed || !strings.Contains(res.Reason, "999999") {
		t.Fatalf("want in-use refusal naming pid, got %+v", res)
	}
	if called {
		t.Error("nothing should be reported as discarded when refused")
	}
	if _, err := os.Stat(dest); err != nil || !branchExists(main, "worktree-force") {
		t.Fatal("refused force must leave worktree and branch")
	}
}

func TestForceCleanNeverRemovesMainCheckout(t *testing.T) {
	repo, main, _ := forceFixture(t)
	if !isMainCheckout(repo, main) {
		t.Fatal("isMainCheckout must recognise the main checkout")
	}
	for _, name := range []string{"..", ".", filepath.Base(main), "../" + filepath.Base(main)} {
		res, err := CleanForce(repo, name, false, nil)
		if err == nil && res.Removed {
			t.Errorf("name %q: main checkout removed", name)
		}
	}
	if _, err := os.Stat(filepath.Join(main, ".git")); err != nil {
		t.Fatalf("main damaged: %v", err)
	}
}
