package gemini

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

func TestGeminiCanceledContext(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, dirs := range [][]string{nil, {"."}} {
		got, err := CollectContext(ctx, dir, dirs, []string{"*.go"}, 40, 100)
		if !errors.Is(err, context.Canceled) || len(got.Included) != 0 {
			t.Errorf("CollectContext(%v) = %+v, %v, want cancellation without files", dirs, got, err)
		}
	}
	if _, err := ChangedFilesVsHEAD(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("ChangedFilesVsHEAD error = %v, want cancellation", err)
	}
	if _, err := diffFiles(ctx, Options{Cwd: dir, Diff: true}); !errors.Is(err, context.Canceled) {
		t.Errorf("diffFiles error = %v, want cancellation", err)
	}
}

func TestGeminiGitCommandCancellation(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not on PATH")
	}
	for _, tc := range []struct {
		name  string
		stage string
		run   func(context.Context, string) (Context, error)
	}{
		{"collect probe", "rev-parse", func(ctx context.Context, dir string) (Context, error) {
			return CollectContext(ctx, dir, []string{"."}, nil, 40, 100)
		}},
		{"collect list", "ls-files", func(ctx context.Context, dir string) (Context, error) {
			return CollectContext(ctx, dir, []string{"."}, nil, 40, 100)
		}},
		{"changed diff", "diff", func(ctx context.Context, dir string) (Context, error) {
			_, err := ChangedFilesVsHEAD(ctx, dir)
			return Context{}, err
		}},
		{"changed list", "ls-files", func(ctx context.Context, dir string) (Context, error) {
			_, err := ChangedFilesVsHEAD(ctx, dir)
			return Context{}, err
		}},
		{"bridge diff", "diff", func(ctx context.Context, dir string) (Context, error) {
			_, err := diffFiles(ctx, Options{Cwd: dir, Diff: true})
			return Context{}, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			marker := filepath.Join(dir, "git-started")
			write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
			write(t, filepath.Join(dir, "ignored.txt"), "must not be inlined")
			script := "#!/bin/sh\n" +
				"if [ \"$3\" = \"" + tc.stage + "\" ]; then\n" +
				"  : > \"$GEMINI_TEST_GIT_MARKER\"\n" +
				"  exec \"" + sleep + "\" 3\n" +
				"fi\n" +
				"case \"$3\" in\n" +
				"  rev-parse) printf 'true\\n' ;;\n" +
				"  diff) printf 'changed.go\\n' ;;\n" +
				"  ls-files) printf 'tracked.go\\000' ;;\n" +
				"  *) exit 1 ;;\n" +
				"esac\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("GEMINI_TEST_GIT_MARKER", marker)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			type result struct {
				context Context
				err     error
			}
			done := make(chan result, 1)
			go func() {
				got, err := tc.run(ctx, dir)
				done <- result{got, err}
			}()
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				select {
				case <-tick.C:
				case got := <-done:
					t.Fatalf("git returned before synchronization: %+v, %v", got.context, got.err)
				case <-deadline.C:
					t.Fatal("git did not reach the cancellation synchronization marker")
				}
			}
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
		})
	}
}

func TestCollectContextGitFailureFallback(t *testing.T) {
	for _, stage := range []string{"rev-parse", "ls-files"} {
		t.Run(stage, func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			write(t, filepath.Join(dir, "a.go"), "package a")
			script := "#!/bin/sh\nif [ \"$3\" = \"" + stage + "\" ]; then exit 1; fi\nprintf 'true\\n'\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			got, err := CollectContext(t.Context(), dir, []string{"."}, nil, 40, 100)
			if err != nil || len(got.Included) != 1 || got.Included[0].Path != "a.go" {
				t.Fatalf("CollectContext = %+v, %v, want ordinary git failure fallback", got, err)
			}
		})
	}
}

func TestChangedFilesCancellationMentionsGit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := ChangedFilesVsHEAD(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "git") {
		t.Fatalf("error = %v, want wrapped git cancellation", err)
	}
}
