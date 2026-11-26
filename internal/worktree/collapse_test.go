package worktree

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollapseLanes(t *testing.T) {
	repo := testRepo(t)
	lane := func(name, file, body string) string {
		wt, err := Create(repo, name, CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		commitIn(t, wt, "add", file)
		commitIn(t, wt, "commit", "-m", "feat: "+name)
		return wt
	}
	lane("one", "a.txt", "a")
	lane("two", "b.txt", "b")
	lane("three", "a.txt", "conflict")
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
	out, err = run("three")
	if !errors.Is(err, ErrCollapseStopped) || !strings.Contains(err.Error(), "three") || !strings.Contains(err.Error(), "a.txt") ||
		!strings.Contains(out, "resume: claude-code worktree collapse col three") || headOf(col) != good {
		t.Fatalf("conflict must stop at the last good state: %v\n%s", err, out)
	}
	dirty := lane("dirty", "d.txt", "d")
	if err := os.WriteFile(filepath.Join(dirty, "d.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = run("dirty"); !errors.Is(err, ErrCollapseStopped) || !strings.Contains(err.Error(), "dirty (uncommitted") {
		t.Fatalf("dirty lane must be refused by name: %v", err)
	}
}
