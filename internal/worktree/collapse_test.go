package worktree

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// collapseLaneFixture creates lane name and commits each file, body pair as its own commit.
func collapseLaneFixture(t *testing.T, repo *Repo, name string, pairs ...string) string {
	t.Helper()
	wt, err := Create(repo, name, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(pairs); i += 2 {
		if err := os.WriteFile(filepath.Join(wt, pairs[i]), []byte(pairs[i+1]), 0o644); err != nil {
			t.Fatal(err)
		}
		commitIn(t, wt, "add", pairs[i])
		commitIn(t, wt, "commit", "-m", "feat: "+name+" "+pairs[i])
	}
	return wt
}

func TestCollapseLanes(t *testing.T) {
	repo := testRepo(t)
	lane := func(name string, pairs ...string) string { return collapseLaneFixture(t, repo, name, pairs...) }
	lane("one", "a.txt", "a")
	lane("two", "b.txt", "b")
	// three's first commit picks clean and its second conflicts: the drop must undo both
	lane("three", "e.txt", "e", "a.txt", "conflict")
	run := func(lanes ...string) (string, error) {
		var out bytes.Buffer
		err := Collapse(context.Background(), repo, CollapseRequest{Collector: "col", Lanes: lanes, Out: &out})
		return out.String(), err
	}
	out, err := run("one", "two")
	if err != nil || !strings.Contains(out, "collapsed two (1 commits); removed") || !strings.Contains(out, "collector col: 2 lanes") {
		t.Fatalf("collapse: %v\n%s", err, out)
	}
	col, _ := resolveWorktreeBounded(repo, "col")
	good := headOf(col)
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(col, f)); err != nil {
			t.Fatalf("collector lacks %s", f)
		}
	}
	lane("four", "c.txt", "c")
	out, err = run("three", "four")
	if !errors.Is(err, ErrCollapseStopped) || !strings.Contains(err.Error(), "three") || !strings.Contains(err.Error(), "a.txt") ||
		!strings.Contains(out, "dropped three") || !strings.Contains(out, "collapsed four (1 commits); removed") || headOf(col) == good {
		t.Fatalf("a conflict must drop its lane and still pick the next: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(col, "e.txt")); err == nil {
		t.Fatal("the dropped lane's first commit stayed in the collector")
	}
	dirty := lane("dirty", "d.txt", "d")
	if err := os.WriteFile(filepath.Join(dirty, "d.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = run("dirty"); !errors.Is(err, ErrCollapseStopped) || !strings.Contains(err.Error(), "dirty (uncommitted") {
		t.Fatalf("dirty lane must be refused by name: %v", err)
	}
}

// A collector with a tracked edit is refused before any pick, so a rollback never resets the edit away.
func TestCollapseRefusesDirtyCollector(t *testing.T) {
	repo := testRepo(t)
	collapseLaneFixture(t, repo, "one", "a.txt", "a")
	collapseLaneFixture(t, repo, "two", "a.txt", "conflict")
	if err := Collapse(t.Context(), repo, CollapseRequest{Collector: "col", Lanes: []string{"one"}, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	col, _ := resolveWorktreeBounded(repo, "col")
	head := headOf(col)
	if err := os.WriteFile(filepath.Join(col, "a.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Collapse(t.Context(), repo, CollapseRequest{Collector: "col", Lanes: []string{"two"}, Out: io.Discard})
	got, _ := os.ReadFile(filepath.Join(col, "a.txt"))
	if !errors.Is(err, ErrCollapseStopped) || !strings.Contains(err.Error(), "uncommitted") || string(got) != "mine" || headOf(col) != head {
		t.Fatalf("dirty collector: err %v, a.txt %q, head moved %v", err, got, headOf(col) != head)
	}
}

func collapseOne(t *testing.T, repo *Repo, dry bool, lanes ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := Collapse(t.Context(), repo, CollapseRequest{Collector: "col", Lanes: lanes, DryRun: dry, Out: &out}); err != nil {
		t.Fatalf("collapse: %v\n%s", err, out.String())
	}
	return out.String()
}

func TestCollapseRemovesTheConvergedLane(t *testing.T) {
	repo := testRepo(t)
	lane := collapseLaneFixture(t, repo, "one", "a.txt", "a")
	out := collapseOne(t, repo, false, "one")
	col, _ := resolveWorktreeBounded(repo, "col")
	if !strings.Contains(out, "collapsed one (1 commits); removed\n") {
		t.Fatalf("output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(col, "a.txt")); err != nil {
		t.Fatal("the lane's commit must be in the collector")
	}
	if _, err := os.Stat(lane); err == nil || branchExists(repo.Main, "worktree-one") {
		t.Fatal("the lane's directory and branch must be gone")
	}
}

func TestCollapseKeepsALaneWithAnUntrackedFile(t *testing.T) {
	repo := testRepo(t)
	lane := collapseLaneFixture(t, repo, "one", "a.txt", "a")
	if err := os.WriteFile(filepath.Join(lane, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := collapseOne(t, repo, false, "one")
	if !strings.Contains(out, "collapsed one (1 commits); kept: uncommitted or untracked files\n") {
		t.Fatalf("output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(lane, "notes.txt")); err != nil || !branchExists(repo.Main, "worktree-one") {
		t.Fatal("the lane and its file must stay")
	}
}

func TestCollapseKeepsALaneWhoseHeadMovedAfterThePick(t *testing.T) {
	repo := testRepo(t)
	lane := collapseLaneFixture(t, repo, "one", "a.txt", "a")
	afterCollapsePick = func(collapseLane) { commitIn(t, lane, "commit", "--allow-empty", "-m", "chore: late") }
	t.Cleanup(func() { afterCollapsePick = func(collapseLane) {} })
	out := collapseOne(t, repo, false, "one")
	if !strings.Contains(out, "collapsed one (1 commits); kept: head moved after the pick\n") || !branchExists(repo.Main, "worktree-one") {
		t.Fatalf("output: %s", out)
	}
}

func TestCollapseNeverRemovesAConflictingLane(t *testing.T) {
	repo := testRepo(t)
	collapseLaneFixture(t, repo, "one", "a.txt", "a")
	collapseLaneFixture(t, repo, "two", "a.txt", "conflict")
	collapseOne(t, repo, false, "one")
	var out bytes.Buffer
	err := Collapse(t.Context(), repo, CollapseRequest{Collector: "col", Lanes: []string{"two"}, Out: &out})
	if !errors.Is(err, ErrCollapseStopped) || !strings.Contains(out.String(), "dropped two: conflict in a.txt") || !branchExists(repo.Main, "worktree-two") {
		t.Fatalf("a conflicting lane must be dropped and kept: %v\n%s", err, out.String())
	}
}

func TestCollapseKeepsALaneWhoseEquivalentPatchWasRevertedInTheCollector(t *testing.T) {
	repo := testRepo(t)
	collapseLaneFixture(t, repo, "one", "a.txt", "a")
	collapseLaneFixture(t, repo, "dup", "a.txt", "a") // the same patch as one's
	collapseOne(t, repo, false, "one")
	col, _ := resolveWorktreeBounded(repo, "col")
	commitIn(t, col, "revert", "--no-edit", "HEAD")
	out := collapseOne(t, repo, false, "dup")
	if !strings.Contains(out, "collapsed dup (0 of 1 commits); kept: content differs from the collector (a.txt)\n") || !branchExists(repo.Main, "worktree-dup") {
		t.Fatalf("output: %s", out)
	}
}

func TestCollapseKeepsALaneWithACommitAlreadyInTheCollectorByPatch(t *testing.T) {
	repo := testRepo(t)
	collapseLaneFixture(t, repo, "one", "a.txt", "a")
	collapseLaneFixture(t, repo, "two", "a.txt", "a", "b.txt", "b") // its first commit repeats one's patch
	collapseOne(t, repo, false, "one")
	out := collapseOne(t, repo, false, "two")
	want := "collapsed two (1 of 2 commits); kept: 1 commits were already in the collector or are merges\n"
	if !strings.Contains(out, want) || !branchExists(repo.Main, "worktree-two") {
		t.Fatalf("output: %s", out)
	}
}

func TestCollapseKeepsALaneWhoseMergeResolutionIsNotInTheCollector(t *testing.T) {
	repo := testRepo(t)
	lane := collapseLaneFixture(t, repo, "m")
	commitIn(t, lane, "checkout", "-q", "-b", "side")
	writeLane(t, lane, "s.txt", "s")
	commitIn(t, lane, "add", "s.txt")
	commitIn(t, lane, "commit", "-m", "feat: side")
	commitIn(t, lane, "checkout", "-q", "worktree-m")
	writeLane(t, lane, "m.txt", "m")
	commitIn(t, lane, "add", "m.txt")
	commitIn(t, lane, "commit", "-m", "feat: m")
	commitIn(t, lane, "merge", "--no-ff", "--no-commit", "side")
	writeLane(t, lane, "evil.txt", "only in the merge")
	commitIn(t, lane, "add", "evil.txt")
	commitIn(t, lane, "commit", "-m", "merge side")
	out := collapseOne(t, repo, false, "m")
	if !strings.Contains(out, "kept: content differs from the collector (evil.txt)\n") || !branchExists(repo.Main, "worktree-m") {
		t.Fatalf("output: %s", out)
	}
}

func writeLane(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollapseDryRunRemovesNothing(t *testing.T) {
	repo := testRepo(t)
	lane := collapseLaneFixture(t, repo, "one", "a.txt", "a")
	out := collapseOne(t, repo, true, "one")
	if !strings.Contains(out, "would remove one\n") {
		t.Fatalf("output: %s", out)
	}
	if _, err := os.Stat(lane); err != nil || !branchExists(repo.Main, "worktree-one") {
		t.Fatal("a dry run must remove nothing")
	}
}
