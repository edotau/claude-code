package worktreecmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/hosttest/gittest"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestMain runs in the hermetic host (hosttest): no live provider pins, vault or vendor CLIs are reachable.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	code := m.Run()
	cleanup()
	gittest.Cleanup()
	os.Exit(code)
}

// captureOutput redirects os.Stdout/os.Stderr around fn and returns fn's exit code plus both streams.
func captureOutput(t *testing.T, fn func() int) (code int, stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var bo, be bytes.Buffer
	var drained sync.WaitGroup
	drained.Go(func() { io.Copy(&bo, ro) }) // drain while fn runs: a full pipe would block its writes forever
	drained.Go(func() { io.Copy(&be, re) })
	os.Stdout, os.Stderr = wo, we
	code = fn()
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	drained.Wait()
	return code, bo.String(), be.String()
}

// closedStdout points os.Stdout at a closed file so every write fails, until the test ends.
func closedStdout(t *testing.T) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "closed-stdout"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	t.Cleanup(func() { os.Stdout = old })
}

func scrubGitEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_PREFIX", "GIT_CEILING_DIRECTORIES",
	} {
		value, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		t.Cleanup(func() {
			if err := os.Setenv(key, value); err != nil {
				t.Errorf("restore %s: %v", key, err)
			}
		})
	}
}

// gitRepoCLI makes a fresh git repo with one commit, env wired so every git invocation has an identity.
func gitRepoCLI(t *testing.T) string {
	t.Helper()
	scrubGitEnv(t)
	return gittest.Repo(t)
}
