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
	done := watchWrites(context.Background(), dir)
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := done(); got == nil || len(*got) != 1 {
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
	done := watchWrites(context.Background(), dir)
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(file, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := done(); got == nil || len(*got) != 1 {
		t.Fatalf("changed = %v, want a.txt: it left the status list but was rewritten", got)
	}
}

// Given a run that changes more files than the cap, the record keeps the cap's worth of paths and ends on the marker.
func TestWatchWritesCapsTheListWithAMarker(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.txt": "a\n"})
	done := watchWrites(context.Background(), dir)
	for i := range writesCap + 5 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := done()
	if got == nil || len(*got) != writesCap+1 || (*got)[writesCap] != WroteTruncated {
		t.Fatalf("list = %d entries, want %d ending in %q", len(*got), writesCap+1, WroteTruncated)
	}
}

// Given a git failure around the run (here a directory that vanished), the snapshot yields no list and never panics.
func TestWatchWritesGitFailureLeavesNil(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.txt": "a\n"})
	done := watchWrites(context.Background(), dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if got := done(); got != nil {
		t.Fatalf("list = %v, want nil", *got)
	}
	if got := watchWrites(context.Background(), filepath.Join(dir, "missing"))(); got != nil {
		t.Fatalf("a missing workdir must record nothing, got %v", *got)
	}
}
