package pack

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testBudget is the old default total (2.5 MiB) for tests that never meant to hit it.
const testBudget = 2560 * 1024

func TestCanceledContext(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, dirs := range [][]string{nil, {"."}} {
		got, err := Collect(ctx, dir, dirs, []string{"*.go"}, 40, 100, testBudget)
		if !errors.Is(err, context.Canceled) || len(got.Included) != 0 {
			t.Errorf("Collect(%v) = %+v, %v, want cancellation without files", dirs, got, err)
		}
	}
	if _, err := ChangedFiles(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("ChangedFiles error = %v, want cancellation", err)
	}
	if _, err := changedOrListed(ctx, Spec{Diff: true}, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("changedOrListed error = %v, want cancellation", err)
	}
}

type cancelCase struct {
	name  string
	stage string
	run   func(context.Context, string) (Context, error)
}

func collectRun(ctx context.Context, dir string) (Context, error) {
	return Collect(ctx, dir, []string{"."}, nil, 40, 100, testBudget)
}

func changedRun(ctx context.Context, dir string) (Context, error) {
	_, err := ChangedFiles(ctx, dir)
	return Context{}, err
}

func changedOrListedRun(ctx context.Context, dir string) (Context, error) {
	_, err := changedOrListed(ctx, Spec{Diff: true}, dir)
	return Context{}, err
}

// fakeGitScript is a git stand-in that touches $PACK_TEST_GIT_MARKER and sleeps when run at stage.
func fakeGitScript(sleep, stage string) string {
	return "#!/bin/sh\n" +
		"if [ \"$3\" = \"" + stage + "\" ]; then\n" +
		"  : > \"$PACK_TEST_GIT_MARKER\"\n" +
		"  exec \"" + sleep + "\" 3\n" +
		"fi\n" +
		"case \"$3\" in\n" +
		"  rev-parse) printf 'true\\n' ;;\n" +
		"  diff) printf 'changed.go\\n' ;;\n" +
		"  ls-files) printf 'tracked.go\\000' ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
}

type cancelResult struct {
	context Context
	err     error
}

// waitForMarker blocks until marker exists, failing if the run returns or 2s pass first.
func waitForMarker(t *testing.T, marker string, done <-chan cancelResult) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		select {
		case <-tick.C:
		case got := <-done:
			t.Fatalf("git returned before synchronization: %+v, %v", got.context, got.err)
		case <-deadline.C:
			t.Fatal("git did not reach the cancellation synchronization marker")
		}
	}
}

func runCancellationCase(t *testing.T, sleep string, tc cancelCase) {
	t.Helper()
	dir, bin := t.TempDir(), t.TempDir()
	marker := filepath.Join(dir, "git-started")
	write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
	write(t, filepath.Join(dir, "ignored.txt"), "must not be inlined")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fakeGitScript(sleep, tc.stage)), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("PACK_TEST_GIT_MARKER", marker)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan cancelResult, 1)
	go func() {
		got, err := tc.run(ctx, dir)
		done <- cancelResult{got, err}
	}()
	waitForMarker(t, marker, done)
	canceledAt := time.Now()
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Errorf("error = %v, want cancellation", got.err)
		}
		if len(got.context.Included) != 0 {
			t.Errorf("canceled scan included files: %+v", got.context.Included)
		}
		if elapsed := time.Since(canceledAt); elapsed > time.Second {
			t.Errorf("git took %s to stop after cancellation", elapsed)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("git command did not stop after cancellation")
	}
}

func TestGitCommandCancellation(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not on PATH")
	}
	for _, tc := range []cancelCase{
		{"collect probe", "rev-parse", collectRun},
		{"collect list", "ls-files", collectRun},
		{"changed diff", "diff", changedRun},
		{"changed list", "ls-files", changedRun},
		{"diff-only spec", "diff", changedOrListedRun},
	} {
		t.Run(tc.name, func(t *testing.T) { runCancellationCase(t, sleep, tc) })
	}
}

func TestCollectGitFailureFallback(t *testing.T) {
	for _, stage := range []string{"rev-parse", "ls-files"} {
		t.Run(stage, func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			write(t, filepath.Join(dir, "a.go"), "package a")
			script := "#!/bin/sh\nif [ \"$3\" = \"" + stage + "\" ]; then exit 1; fi\nprintf 'true\\n'\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			got, err := Collect(t.Context(), dir, []string{"."}, nil, 40, 100, testBudget)
			if stage == "ls-files" {
				if err == nil || !strings.Contains(err.Error(), "git ls-files") {
					t.Fatalf("Collect = %+v, %v, want fail-closed ls-files error", got, err)
				}
				return
			}
			if err != nil || len(got.Included) != 1 || got.Included[0].Path != "a.go" {
				t.Fatalf("Collect = %+v, %v, want rev-parse failure fallback", got, err)
			}
		})
	}
}

func TestChangedFilesCancellationMentionsGit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := ChangedFiles(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "git") {
		t.Fatalf("error = %v, want wrapped git cancellation", err)
	}
}
