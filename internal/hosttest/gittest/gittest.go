// Package gittest hands tests a fresh one-commit git repo copied from a template built once per test binary.
// Imports stdlib only, so any package's tests may use it.
package gittest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

var (
	seq     atomic.Int64
	once    sync.Once
	tmpl    string
	tmplErr error
)

// Cleanup removes the template repo; call it from TestMain after m.Run.
func Cleanup() {
	if tmpl != "" {
		_ = os.RemoveAll(tmpl)
	}
}

// Repo returns a fresh repo on branch main with one empty commit, in a globally unique directory name.
func Repo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), fmt.Sprintf("repo%d", seq.Add(1)))
	RepoAt(t, dir)
	return dir
}

// RepoAt makes dir (and parents) a repo like Repo's, for callers that need a chosen directory name.
func RepoAt(t *testing.T, dir string) string {
	t.Helper()
	once.Do(buildTemplate)
	if tmplErr != nil {
		t.Fatal(tmplErr)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(context.Background(), "cp", "-a", tmpl+"/.", dir).CombinedOutput(); err != nil {
		t.Fatalf("copy template repo: %v %s", err, out)
	}
	return dir
}

func buildTemplate() {
	if tmpl, tmplErr = os.MkdirTemp("", "gittest-"); tmplErr != nil {
		return
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_PREFIX":
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "--allow-empty", "-q", "-m", "init"}} {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = tmpl
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			tmplErr = fmt.Errorf("git %v: %v: %s", args, err, out)
			return
		}
	}
}
