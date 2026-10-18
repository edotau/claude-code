package hooks

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepo makes a fresh git repo with one commit, env wired so every git invocation has an identity.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "--allow-empty", "-q", "-m", "init")
	return dir
}

// createPayload is the real WorktreeCreate shape Claude Code sends: only cwd + name.
func createPayload(cwd, name string) *strings.Reader {
	b, _ := json.Marshal(map[string]any{
		"session_id": "s1", "cwd": cwd, "hook_event_name": "WorktreeCreate", "name": name,
	})
	return strings.NewReader(string(b))
}

func removePayload(cwd, worktreePath string) *strings.Reader {
	b, _ := json.Marshal(map[string]any{
		"session_id": "s1", "cwd": cwd, "hook_event_name": "WorktreeRemove", "worktree_path": worktreePath,
	})
	return strings.NewReader(string(b))
}

func TestWorktreeCreateFromRealPayload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)

	var out, errb bytes.Buffer
	if code := WorktreeCreate(createPayload(repo, "live-probe"), &out, &errb); code != ExitProceed {
		t.Fatalf("create: got %d, stderr %s", code, errb.String())
	}
	want := filepath.Join(home, "worktrees", filepath.Base(repo), "live-probe")
	got := strings.TrimSpace(out.String())
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("worktree dir missing: %v", err)
	}
	branchOut, err := exec.Command("git", "-C", got, "branch", "--show-current").Output()
	if err != nil || strings.TrimSpace(string(branchOut)) != "worktree-live-probe" {
		t.Fatalf("branch = %q, err %v", branchOut, err)
	}
}

func TestWorktreeCreateRepeatReturnsSamePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)

	var out1 bytes.Buffer
	if code := WorktreeCreate(createPayload(repo, "dup"), &out1, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("first create: got %d", code)
	}
	var out2, errb2 bytes.Buffer
	if code := WorktreeCreate(createPayload(repo, "dup"), &out2, &errb2); code != ExitProceed {
		t.Fatalf("second create: got %d, stderr %s", code, errb2.String())
	}
	if strings.TrimSpace(out1.String()) != strings.TrimSpace(out2.String()) {
		t.Fatalf("paths differ: %q vs %q", out1.String(), out2.String())
	}
}

func TestWorktreeCreateFromInsideLinkedWorktreeResolvesToMain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)

	var out bytes.Buffer
	if code := WorktreeCreate(createPayload(repo, "first"), &out, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("create: got %d", code)
	}
	linked := strings.TrimSpace(out.String())

	// A second worktree requested with cwd set to the first linked checkout must still land under the main repo.
	var out2, errb2 bytes.Buffer
	if code := WorktreeCreate(createPayload(linked, "second"), &out2, &errb2); code != ExitProceed {
		t.Fatalf("create from linked cwd: got %d, stderr %s", code, errb2.String())
	}
	want := filepath.Join(home, "worktrees", filepath.Base(repo), "second")
	if got := strings.TrimSpace(out2.String()); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestWorktreeCreateNonRepoCWD(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	notRepo := t.TempDir()
	var errb bytes.Buffer
	if code := WorktreeCreate(createPayload(notRepo, "x"), &bytes.Buffer{}, &errb); code != 1 {
		t.Fatalf("got %d, want 1; stderr %s", code, errb.String())
	}
}

func TestWorktreeCreateBadName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepo(t)
	for _, name := range []string{"", "a/b", ".."} {
		var errb bytes.Buffer
		if code := WorktreeCreate(createPayload(repo, name), &bytes.Buffer{}, &errb); code != 1 {
			t.Errorf("name %q: got %d, want 1; stderr %s", name, code, errb.String())
		}
	}
}

func TestWorktreeRemoveClean(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)

	var out bytes.Buffer
	if code := WorktreeCreate(createPayload(repo, "clean"), &out, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("create: got %d", code)
	}
	wt := strings.TrimSpace(out.String())

	var errb bytes.Buffer
	if code := WorktreeRemove(removePayload(wt, wt), &errb); code != ExitProceed {
		t.Fatalf("remove: got %d, stderr %s", code, errb.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree dir should be gone, stat err = %v", err)
	}
	out2, err := exec.Command("git", "-C", repo, "branch", "--list", "worktree-clean").Output()
	if err != nil || strings.TrimSpace(string(out2)) != "" {
		t.Fatalf("merged branch should be deleted, branch --list = %q, err %v", out2, err)
	}
}

func TestWorktreeRemoveDirty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)

	var out bytes.Buffer
	if code := WorktreeCreate(createPayload(repo, "dirty"), &out, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("create: got %d", code)
	}
	wt := strings.TrimSpace(out.String())
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("uncommitted"), 0o600); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	if code := WorktreeRemove(removePayload(wt, wt), &errb); code != 1 {
		t.Fatalf("remove dirty: got %d, stderr %s", code, errb.String())
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("dirty worktree dir should stay, stat err = %v", err)
	}
}

func TestWorktreeRemoveKeepsUnmergedBranch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)

	var out bytes.Buffer
	if code := WorktreeCreate(createPayload(repo, "unmerged"), &out, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("create: got %d", code)
	}
	wt := strings.TrimSpace(out.String())

	commit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = wt
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	commit("commit", "--allow-empty", "-q", "-m", "unmerged work")

	var errb bytes.Buffer
	if code := WorktreeRemove(removePayload(wt, wt), &errb); code != ExitProceed {
		t.Fatalf("remove: got %d, stderr %s", code, errb.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree dir should be gone, stat err = %v", err)
	}
	branchOut, err := exec.Command("git", "-C", repo, "branch", "--list", "worktree-unmerged").Output()
	if err != nil || strings.TrimSpace(string(branchOut)) == "" {
		t.Fatalf("unmerged branch should be kept, branch --list = %q, err %v", branchOut, err)
	}
}
