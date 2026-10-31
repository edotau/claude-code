package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/worktree"
)

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

// gitRepo makes a fresh git repo with one commit, env wired so every git invocation has an identity.
func gitRepo(t *testing.T) string {
	t.Helper()
	scrubGitEnv(t)
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
	if code := runDirect("worktree-create", createPayload(repo, "live-probe"), &out, &errb); code != ExitProceed {
		t.Fatalf("create: got %d, stderr %s", code, errb.String())
	}
	want := filepath.Join(canonHome(t, home), "worktrees", filepath.Base(repo), "live-probe")
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
	if code := runDirect("worktree-create", createPayload(repo, "dup"), &out1, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("first create: got %d", code)
	}
	var out2, errb2 bytes.Buffer
	if code := runDirect("worktree-create", createPayload(repo, "dup"), &out2, &errb2); code != ExitProceed {
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
	if code := runDirect("worktree-create", createPayload(repo, "first"), &out, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("create: got %d", code)
	}
	linked := strings.TrimSpace(out.String())

	// A second worktree requested with cwd set to the first linked checkout must still land under the main repo.
	var out2, errb2 bytes.Buffer
	if code := runDirect("worktree-create", createPayload(linked, "second"), &out2, &errb2); code != ExitProceed {
		t.Fatalf("create from linked cwd: got %d, stderr %s", code, errb2.String())
	}
	want := filepath.Join(canonHome(t, home), "worktrees", filepath.Base(repo), "second")
	if got := strings.TrimSpace(out2.String()); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestWorktreeCreateNonRepoCWD(t *testing.T) {
	scrubGitEnv(t)
	t.Setenv("HOME", t.TempDir())
	notRepo := t.TempDir()
	var errb bytes.Buffer
	if code := runDirect("worktree-create", createPayload(notRepo, "x"), &bytes.Buffer{}, &errb); code != 1 {
		t.Fatalf("got %d, want 1; stderr %s", code, errb.String())
	}
}

func TestWorktreeCreateBadName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepo(t)
	for _, name := range []string{"", "a/b", ".."} {
		var errb bytes.Buffer
		if code := runDirect("worktree-create", createPayload(repo, name), &bytes.Buffer{}, &errb); code != 1 {
			t.Errorf("name %q: got %d, want 1; stderr %s", name, code, errb.String())
		}
	}
}

func TestWorktreeRemoveClean(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)

	var out bytes.Buffer
	if code := runDirect("worktree-create", createPayload(repo, "clean"), &out, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("create: got %d", code)
	}
	wt := strings.TrimSpace(out.String())

	var errb bytes.Buffer
	if code := runDirect("worktree-remove", removePayload(wt, wt), io.Discard, &errb); code != ExitProceed {
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

func TestWorktreeRemoveClosesLedger(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := gitRepo(t)

	var out bytes.Buffer
	if code := runDirect("worktree-create", createPayload(repo, "led"), &out, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("create: got %d", code)
	}
	wt := strings.TrimSpace(out.String())
	ledger := filepath.Join(memory.Dir(repo), "worktrees", "led.md")
	if data, err := os.ReadFile(ledger); err != nil || !strings.Contains(string(data), "- Status: open") {
		t.Fatalf("ledger not opened by create: %v\n%s", err, data)
	}

	var errb bytes.Buffer
	if code := runDirect("worktree-remove", removePayload(wt, wt), io.Discard, &errb); code != ExitProceed {
		t.Fatalf("remove: got %d, stderr %s", code, errb.String())
	}
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- Status: closed", "- Via: remove", "Branch: deleted"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("ledger missing %q:\n%s", want, data)
		}
	}
}

func TestWorktreeRemoveDirty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)

	var out bytes.Buffer
	if code := runDirect("worktree-create", createPayload(repo, "dirty"), &out, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("create: got %d", code)
	}
	wt := strings.TrimSpace(out.String())
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("uncommitted"), 0o600); err != nil {
		t.Fatal(err)
	}

	var errb bytes.Buffer
	if code := runDirect("worktree-remove", removePayload(wt, wt), io.Discard, &errb); code != 1 {
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
	if code := runDirect("worktree-create", createPayload(repo, "unmerged"), &out, &bytes.Buffer{}); code != ExitProceed {
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
	if code := runDirect("worktree-remove", removePayload(wt, wt), io.Discard, &errb); code != ExitProceed {
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

// TestWorktreeRemoveRefusesWhenCWDIsInside is the hook-path regression test for InUseBy's cwd guard:
// WorktreeRemove must refuse (and leave the worktree in place) when the calling process's own cwd is
// still inside the worktree being removed, the one in-use case this package can trigger without
// stubbing worktree's unexported process-discovery seam.
func TestWorktreeRemoveRefusesWhenCWDIsInside(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := gitRepo(t)

	var out bytes.Buffer
	if code := runDirect("worktree-create", createPayload(repo, "cwdbusy"), &out, &bytes.Buffer{}); code != ExitProceed {
		t.Fatalf("create: got %d", code)
	}
	wt := strings.TrimSpace(out.String())
	t.Chdir(wt)

	var errb bytes.Buffer
	if code := runDirect("worktree-remove", removePayload(wt, wt), io.Discard, &errb); code != 1 {
		t.Fatalf("remove: got %d, want 1 (refused); stderr %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "cd out") {
		t.Errorf("stderr = %q, want it to mention cd-ing out", errb.String())
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree should still exist after a refused remove: %v", err)
	}
}

// canonHome resolves a temp HOME the way WorktreeRoot does (macOS tmp dirs live behind a /var symlink).
func canonHome(t *testing.T, home string) string {
	t.Helper()
	c, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// An update in a linked worktree logs one ledger line.
func TestUpdateAppendsWorktreeLedger(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home+"/.claude")
	t.Setenv(memory.UpdateChildEnv, "")
	main := filepath.Join(home, "repo")
	for _, a := range [][]string{{"init", "-q", "-b", "main", main}, {"-C", main, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-q", "-m", "i"}} {
		if out, err := exec.Command("git", a...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	repo, err := worktree.ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := worktree.Create(repo, "wt-h", worktree.CreateOptions{Log: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	stubSpawn(t, true)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 3)
	if sessionUpdate(ParseInput(strings.NewReader(updatePayload(tr, dest))), &bytes.Buffer{}) != ExitProceed {
		t.Fatal("update blocked")
	}
	data, err := os.ReadFile(filepath.Join(memory.Dir(main), "worktrees", "wt-h.md"))
	if err != nil || !strings.Contains(string(data), "update (") {
		t.Fatalf("no update line in ledger: %v\n%s", err, data)
	}
}

// A failing worktree handler must fail the dispatcher: Claude Code reads exit 0 as "worktree created".
func TestWorktreeFailureKeepsExitCode(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	orig := Native["worktree-create"]
	Native["worktree-create"] = func(*Input) Result { return Result{Stderr: "worktree-create: boom", ExitCode: 1} }
	t.Cleanup(func() { Native["worktree-create"] = orig })

	var out, errb strings.Builder
	code := RunEvent("WorktreeCreate", false, strings.NewReader(`{"hook_event_name":"WorktreeCreate","name":"x"}`), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "boom") || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q; want exit 1, empty stdout, stderr kept", code, out.String(), errb.String())
	}
}

// TestWorktreeModeAndBusy: off|false|0 is silent even with a peer; unset asks at start and reports a changed peer set once.
func TestWorktreeModeAndBusy(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	peers := []worktree.Proc{{PID: 4242, Comm: "claude"}}
	old := peerAgents
	peerAgents = func(string, []string) ([]worktree.Proc, error) { return peers, nil }
	defer func() { peerAgents = old }()
	in := &Input{SessionID: "s1", CWD: cwd}

	for _, v := range []string{"off", "false", "0", "OFF"} {
		t.Setenv(worktreeModeEnv, v)
		if got := worktreeMode(in).Context + worktreeBusy(in).Context; got != "" {
			t.Fatalf("%s must be silent with a peer present, got %q", v, got)
		}
	}
	t.Setenv(worktreeModeEnv, "")
	if got := worktreeMode(in).Context; got != worktreeAskLine {
		t.Fatalf("unset start: %q", got)
	}
	if got := worktreeBusy(in).Context; !strings.Contains(got, "claude pid 4242") {
		t.Fatalf("unset busy: %q", got)
	}
	if got := worktreeBusy(in).Context; got != "" {
		t.Fatalf("same peers must dedupe, got %q", got)
	}
	peers = []worktree.Proc{{PID: 4242, Comm: "claude"}, {PID: 77, Comm: "codex"}}
	if got := worktreeBusy(in).Context; got == "" {
		t.Fatal("changed peer set must re-report")
	}
	peers = nil
	if got := worktreeBusy(in).Context; got != "" {
		t.Fatalf("no peers: %q", got)
	}
}

// TestWorktreeBusyStateFileIs0600: the busy-peer state file is private to the owner.
func TestWorktreeBusyStateFileIs0600(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(worktreeModeEnv, "")
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	old := peerAgents
	peerAgents = func(string, []string) ([]worktree.Proc, error) {
		return []worktree.Proc{{PID: 4242, Comm: "claude"}}, nil
	}
	defer func() { peerAgents = old }()
	worktreeBusy(&Input{SessionID: "s1", CWD: cwd})
	files, _ := filepath.Glob(filepath.Join(paths.StateDir(), "worktree-busy", "*"))
	if len(files) != 1 {
		t.Fatalf("want one state file, got %v", files)
	}
	st, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v; want 0600", st.Mode().Perm())
	}
}
