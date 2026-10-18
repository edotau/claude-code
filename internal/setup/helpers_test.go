package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// mustWrite writes content to path, creating parent dirs; shared across clash/nuke/adopt fixtures.
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitInit creates a git repo at dir, writes files (relative path -> content), and commits them.
func gitInit(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.co", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.co")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	for rel, content := range files {
		mustWrite(t, filepath.Join(dir, rel), content)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
}
