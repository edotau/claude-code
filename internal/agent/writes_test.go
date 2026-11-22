package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
)

// actEngine runs act inside the run, as an agent that writes files would.
type actEngine struct{ act func() }

func (*actEngine) Name() string     { return "fake" }
func (*actEngine) Caps() Caps       { return Caps{} }
func (*actEngine) Available() error { return nil }
func (e *actEngine) Run(context.Context, RunOptions, wire.Sink) (wire.Result, error) {
	e.act()
	return wire.Result{Answer: "a"}, nil
}

// execRecorded runs one dispatch of act in workdir and returns its saved record.
func execRecorded(t *testing.T, workdir string, act func()) Record {
	t.Helper()
	p := streamPrepared(t, &actEngine{act: act})
	p.Opts.WorkDir = workdir
	if _, err := (&Client{}).Exec(context.Background(), p, wire.Discard, RunRecordedOpts{}); err != nil {
		t.Fatal(err)
	}
	rec, err := LoadRecord(p.Record.ID)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// Given a repo with staged files, the record lists exactly the files the run changed, created or deleted.
func TestExecRecordsTheFilesTheRunChanged(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n", "gone.txt": "g\n"})
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Error(err)
		}
	}
	rec := execRecorded(t, dir, func() {
		write("a.txt", "a, rewritten\n")
		write("c.txt", "new\n")
		if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
			t.Error(err)
		}
	})
	want := []string{filepath.Join(real, "a.txt"), filepath.Join(real, "c.txt"), filepath.Join(real, "gone.txt")}
	if rec.Wrote == nil || !slices.Equal(*rec.Wrote, want) {
		t.Fatalf("Wrote = %v, want %v (b.txt is dirty but untouched)", rec.Wrote, want)
	}
	if rec = execRecorded(t, dir, func() {}); rec.Wrote == nil || len(*rec.Wrote) != 0 {
		t.Fatalf("a run that wrote nothing records an empty list, got %v", rec.Wrote)
	}
}

// Given a directory that is no git repository, nothing was observed, so the record carries no list.
func TestExecRecordsNoWritesOutsideARepo(t *testing.T) {
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(t.TempDir()))
	if rec := execRecorded(t, t.TempDir(), func() {}); rec.Wrote != nil {
		t.Fatalf("Wrote = %v, want nil", *rec.Wrote)
	}
}

// Given a rewrite that keeps the file's size, the changed write time still marks it.
func TestWatchWritesSeesASameSizeRewrite(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.txt": "one\n"})
	done := watchWrites(context.Background(), dir, "")
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := done(); got == nil || len(*got) != 1 {
		t.Fatalf("changed = %v, want a.txt", got)
	}
}

// Given a file already modified before the run, a run that restores it still counts as having written it.
func TestWatchWritesSeesAFileTheRunMadeClean(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.txt": "one\n"})
	git := func(args ...string) {
		t.Helper()
		base := []string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}
		out, err := exec.Command("git", append(base, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("commit", "-q", "-m", "base")
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := watchWrites(context.Background(), dir, "")
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(file, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := done(); got == nil || len(*got) != 1 {
		t.Fatalf("changed = %v, want a.txt: it left the status list but was rewritten", got)
	}
}

// Given a run that changes more files than the cap, the record keeps the cap's worth of paths and ends on the marker.
func TestWatchWritesCapsTheListWithAMarker(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.txt": "a\n"})
	done := watchWrites(context.Background(), dir, "")
	for i := range writesCap + 5 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := done()
	if got == nil || len(*got) != writesCap+1 || (*got)[writesCap] != WroteTruncated {
		t.Fatalf("list = %d entries, want %d ending in %q", len(*got), writesCap+1, WroteTruncated)
	}
}

// Given a git failure around the run (here a directory that vanished), the snapshot yields no list and never panics.
func TestWatchWritesGitFailureLeavesNil(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.txt": "a\n"})
	done := watchWrites(context.Background(), dir, "")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := done(); got != nil {
		t.Fatalf("list = %v, want nil", *got)
	}
	if got := func() *[]string {
		g, _ := watchWrites(context.Background(), filepath.Join(dir, "missing"), "")()
		return g
	}(); got != nil {
		t.Fatalf("a missing workdir must record nothing, got %v", *got)
	}
}

// Given a peer dispatch that ran in the same tree during the run, the run claims no file as its own.
func TestExecAttributesWritesOnlyWhenAloneInTheTree(t *testing.T) {
	for _, tc := range []struct {
		name, peerTree string // peerTree "" = no peer record; "same" = this repo's tree; else another tree
	}{{"solo run", ""}, {"peer in another tree", "/elsewhere"}, {"peer in the same tree", "same"}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := gitRepoWith(t, map[string]string{"a.txt": "a\n"})
			tree := paths.GitTop(context.Background(), dir)
			p := streamPrepared(t, &actEngine{act: func() {
				if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("peer wrote\n"), 0o600); err != nil {
					t.Error(err)
				}
			}})
			p.Opts.WorkDir, p.Record.Tree = dir, tree
			if tc.peerTree != "" {
				peer := NewRecord("codex", RunOptions{}, p.Record.Time.Add(-time.Minute))
				peer.Status, peer.PID, peer.Tree = "running", os.Getpid(), tc.peerTree
				if tc.peerTree == "same" {
					peer.Tree = tree
				}
				if err := peer.Save(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := (&Client{}).Exec(context.Background(), p, wire.Discard, RunRecordedOpts{}); err != nil {
				t.Fatal(err)
			}
			rec, err := LoadRecord(p.Record.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{filepath.Join(tree, "a.txt")}
			if tc.peerTree == "same" {
				if rec.Wrote != nil || rec.TreeChanged == nil || !slices.Equal(*rec.TreeChanged, want) {
					t.Fatalf("Wrote = %v, TreeChanged = %v, want nil and %v", rec.Wrote, rec.TreeChanged, want)
				}
				return
			}
			if rec.TreeChanged != nil || rec.Wrote == nil || !slices.Equal(*rec.Wrote, want) {
				t.Fatalf("Wrote = %v, TreeChanged = %v, want %v and nil", rec.Wrote, rec.TreeChanged, want)
			}
		})
	}
}

// Given files dirty before the run, only those the run changed or reverted are listed as pre-existing touched.
func TestWatchWritesListsPreDirtyFilesTheRunTouched(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"edited.txt": "1\n", "idle.txt": "1\n", "reverted.txt": "1\n", "clean.txt": "1\n"})
	git := func(args ...string) {
		t.Helper()
		base := []string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}
		if out, err := exec.Command("git", append(base, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("commit", "-q", "-m", "base")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"edited.txt", "idle.txt", "reverted.txt"} {
		write(n, "dirty\n")
	}
	done := watchWrites(context.Background(), dir, "")
	time.Sleep(10 * time.Millisecond)
	write("edited.txt", "again, longer\n")
	write("reverted.txt", "1\n")
	write("clean.txt", "2\n")
	_, touched := done()
	if touched == nil || len(*touched) != 2 || filepath.Base((*touched)[0]) != "edited.txt" ||
		filepath.Base((*touched)[1]) != "reverted.txt" {
		t.Fatalf("touched = %v, want edited.txt and reverted.txt only", touched)
	}
	if _, none := watchWrites(context.Background(), dir, "")(); none != nil {
		t.Fatalf("a run touching no pre-dirty file must report nil, got %v", *none)
	}
}

// Given a tree that contains the workdir, it is the root watched: no git spawn picks the nested repo instead.
func TestWatchWritesUsesATreeThatHoldsTheWorkdir(t *testing.T) {
	outer := gitRepoWith(t, map[string]string{"a.txt": "a\n"})
	nested := filepath.Join(outer, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", nested, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	done := watchWrites(context.Background(), nested, outer)
	if err := os.WriteFile(filepath.Join(outer, "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := done(); got == nil || len(*got) != 1 || filepath.Base((*got)[0]) != "new.txt" {
		t.Fatalf("changed = %v, want new.txt from the passed tree", got)
	}
}

// Given a tree that does not contain the workdir, it is ignored and the workdir's own repository is watched.
func TestWatchWritesIgnoresATreeOutsideTheWorkdir(t *testing.T) {
	other := gitRepoWith(t, map[string]string{"a.txt": "a\n"})
	work := gitRepoWith(t, map[string]string{"b.txt": "b\n"})
	done := watchWrites(context.Background(), work, other)
	for _, f := range []string{filepath.Join(other, "elsewhere.txt"), filepath.Join(work, "mine.txt")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := done(); got == nil || len(*got) != 1 || filepath.Base((*got)[0]) != "mine.txt" {
		t.Fatalf("changed = %v, want only mine.txt from the workdir's repo", got)
	}
}

// Given proven receipts, they join Wrote on top of a truncated or absent git list; relative paths resolve to the workdir.
func TestMergeWrittenAddsProvenPaths(t *testing.T) {
	got := mergeWritten(nil, []string{"/w/b.txt", "a.txt", "/w/b.txt"}, "/w")
	if got == nil || !slices.Equal(*got, []string{"/w/a.txt", "/w/b.txt"}) {
		t.Fatalf("mergeWritten = %v", got)
	}
	if mergeWritten(nil, nil, "/w") != nil {
		t.Fatal("no receipts must leave an unobserved list nil")
	}
}

// Given the same file spelled through a symlinked directory, When merged, Then Wrote lists it once.
func TestMergeWrittenDedupsSymlinkSpellings(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	f := filepath.Join(real, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := mergeWritten(nil, []string{f, filepath.Join(link, "a.txt")}, real)
	if got == nil || len(*got) != 1 {
		t.Fatalf("mergeWritten = %v, want one path", got)
	}
}
