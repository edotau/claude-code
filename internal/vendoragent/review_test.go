package vendoragent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initRepo creates a git repo at t.TempDir() with one commit on main; runGit is a tiny helper that
// fails the test on error so call sites stay single-line.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestParseScope(t *testing.T) {
	cases := []struct {
		name                   string
		uncommitted            bool
		base, commit           string
		pr                     bool
		wantKind, wantRef, err string
	}{
		{name: "none defaults to uncommitted", wantKind: "uncommitted"},
		{name: "uncommitted alone", uncommitted: true, wantKind: "uncommitted"},
		{name: "base alone", base: "main", wantKind: "base", wantRef: "main"},
		{name: "commit alone", commit: "abc123", wantKind: "commit", wantRef: "abc123"},
		{name: "pr alone", pr: true, wantKind: "pr"},
		{name: "two set errors", uncommitted: true, base: "main", err: "at most one"},
		{name: "base and commit both set errors", base: "main", commit: "abc", err: "at most one"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := ParseScope(c.uncommitted, c.base, c.commit, c.pr)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want containing %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if s.Kind != c.wantKind || s.Ref != c.wantRef {
				t.Errorf("ParseScope = %+v, want Kind=%q Ref=%q", s, c.wantKind, c.wantRef)
			}
		})
	}
}

func TestDiffUncommitted(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("untracked content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := Diff(context.Background(), dir, Scope{Kind: "uncommitted"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "two") {
		t.Errorf("diff missing the HEAD-relative edit:\n%s", s)
	}
	if !strings.Contains(s, "new file: new.txt") || !strings.Contains(s, "untracked content") {
		t.Errorf("diff missing the untracked file block:\n%s", s)
	}
}

func TestDiffUncommittedNothingInScope(t *testing.T) {
	dir := initRepo(t)
	if _, err := Diff(context.Background(), dir, Scope{Kind: "uncommitted"}); err == nil || err.Error() != "nothing in scope" {
		t.Fatalf("err = %v, want \"nothing in scope\"", err)
	}
}

func TestDiffBase(t *testing.T) {
	dir := initRepo(t)
	runGit(t, dir, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nfeature line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "feature work")

	out, err := Diff(context.Background(), dir, Scope{Kind: "base", Ref: "main"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(string(out), "feature line") {
		t.Errorf("base diff missing the feature commit:\n%s", out)
	}
}

func TestDiffCommit(t *testing.T) {
	dir := initRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "second")
	sha := strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))

	out, err := Diff(context.Background(), dir, Scope{Kind: "commit", Ref: sha})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(string(out), "two") {
		t.Errorf("commit diff missing the change:\n%s", out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// pr scope resolves origin/HEAD without a real remote by faking the ref directly.
func TestDiffPR(t *testing.T) {
	dir := initRepo(t)
	runGit(t, dir, "update-ref", "refs/remotes/origin/HEAD", "refs/heads/main")
	runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runGit(t, dir, "update-ref", "refs/remotes/origin/main", "refs/heads/main")
	runGit(t, dir, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\npr line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	runGit(t, dir, "commit", "-q", "-m", "pr work")

	out, err := Diff(context.Background(), dir, Scope{Kind: "pr"})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(string(out), "pr line") {
		t.Errorf("pr diff missing the feature commit:\n%s", out)
	}
}

func TestReviewPromptAdversarialVerdictMarker(t *testing.T) {
	p := ReviewPrompt(true, "")
	if !strings.Contains(p, "VERDICT: no actionable findings") {
		t.Errorf("adversarial prompt missing its verdict marker:\n%s", p)
	}
}

func TestReviewPromptNeutralAsksForVerdictFirst(t *testing.T) {
	p := ReviewPrompt(false, "")
	if !strings.Contains(p, "SHIP IT") || !strings.Contains(p, "NEEDS WORK") || !strings.Contains(p, "BLOCK") {
		t.Errorf("neutral prompt missing verdict vocabulary:\n%s", p)
	}
}

func TestReviewPromptFocusAppended(t *testing.T) {
	p := ReviewPrompt(true, "the auth module")
	if !strings.HasSuffix(p, "Focus: the auth module") {
		t.Errorf("focus not appended:\n%s", p)
	}
}

// ReviewArgs's temp file must survive for a detached background run, and be cleaned up immediately
// for a foreground run whose lifetime ends with this call.
func TestReviewArgsCleanupLifecycle(t *testing.T) {
	for _, background := range []bool{false, true} {
		args, cleanup, err := ReviewArgs("codex", Scope{Kind: "uncommitted"}, []byte("diff content"), true, background, "")
		if err != nil {
			t.Fatalf("ReviewArgs(background=%v): %v", background, err)
		}
		idx := -1
		for i, a := range args {
			if a == "--attach" {
				idx = i + 1
			}
		}
		if idx < 0 || idx >= len(args) {
			t.Fatalf("args = %v, missing --attach", args)
		}
		tmp := args[idx]
		data, err := os.ReadFile(tmp)
		if err != nil || string(data) != "diff content" {
			t.Fatalf("temp file content = %q, err %v, want %q", data, err, "diff content")
		}
		cleanup()
		_, statErr := os.Stat(tmp)
		if background {
			if statErr != nil {
				t.Errorf("background cleanup removed the attach file it must leave for the detached child: %v", statErr)
			}
			os.Remove(tmp) // test cleanup only
		} else {
			if statErr == nil {
				t.Error("foreground cleanup left the temp attach file behind")
			}
		}
	}
}

func TestReviewArgsShape(t *testing.T) {
	args, cleanup, err := ReviewArgs("codex", Scope{Kind: "uncommitted"}, []byte("d"), false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(args) < 6 || args[0] != "--agent" || args[1] != "codex" || args[2] != "--fallback" || args[3] != "vendors" || args[4] != "--attach" {
		t.Fatalf("args = %v, want [--agent codex --fallback vendors --attach <tmp> <prompt>]", args)
	}
	if args[len(args)-1] != ReviewPrompt(false, "") {
		t.Errorf("last arg = %q, want the review prompt", args[len(args)-1])
	}
	for _, a := range args {
		if a == "--background" {
			t.Error("foreground run must not carry --background")
		}
	}
}

func TestReviewArgsBackgroundFlag(t *testing.T) {
	args, cleanup, err := ReviewArgs("codex", Scope{Kind: "uncommitted"}, []byte("d"), false, true, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, a := range args {
			if strings.HasSuffix(a, ".diff") {
				os.Remove(a)
			}
		}
	})
	_ = cleanup
	found := false
	for _, a := range args {
		if a == "--background" {
			found = true
		}
	}
	if !found {
		t.Errorf("args = %v, missing --background", args)
	}
}
