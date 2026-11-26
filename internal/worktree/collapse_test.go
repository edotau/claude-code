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
	if err != nil || !strings.Contains(out, "picked two: 1 commits, 1 files") || !strings.Contains(out, "collector col: 2 lanes") {
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
		!strings.Contains(out, "dropped three") || !strings.Contains(out, "picked four") || headOf(col) == good {
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
