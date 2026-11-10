package hosttest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time" // tests refuse real DNS: mock API calls

	_ "github.com/edotau/claude-code/internal/nonet"
)

func TestRepoCopyInheritsLocalIdentity(t *testing.T) {
	repo := Repo(t)
	for _, tc := range []struct {
		key, want string
	}{
		{"user.name", "t"},
		{"user.email", "t@t.com"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			if got := strings.TrimSpace(In(t, repo, "config", "--local", "--get", tc.key)); got != tc.want {
				t.Fatalf("copied repo config %s = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

func TestInIgnoresLeakedGitDir(t *testing.T) {
	outer := Repo(t)
	inner := Repo(t)
	t.Setenv("GIT_DIR", outer+"/.git")

	In(t, inner, "branch", "leak-probe")

	if got := In(t, inner, "branch", "--list", "leak-probe"); !strings.Contains(got, "leak-probe") {
		t.Fatalf("branch missing from the -C repo: %q", got)
	}
	t.Setenv("GIT_DIR", "")
	if got := In(t, outer, "branch", "--list", "leak-probe"); strings.Contains(got, "leak-probe") {
		t.Fatalf("branch leaked into the repo named by GIT_DIR: %q", got)
	}
}

func TestBuildTemplateIgnoresGlobalGitConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, []byte("[commit]\n\tgpgsign = true\n[gpg]\n\tprogram = /bin/false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	oldTmpl, oldErr := tmpl, tmplErr
	t.Cleanup(func() {
		_ = os.RemoveAll(tmpl)
		tmpl, tmplErr = oldTmpl, oldErr
	})

	buildTemplate()

	if tmplErr != nil {
		t.Fatalf("template build read the user's global git config: %v", tmplErr)
	}
}

func TestInReturnsStdoutOnly(t *testing.T) {
	repo := Repo(t)

	if got := In(t, repo, "checkout", "-b", "stderr-probe"); got != "" {
		t.Fatalf("stderr leaked into the returned output: %q", got)
	}
	if got := strings.TrimSpace(In(t, repo, "branch", "--show-current")); got != "stderr-probe" {
		t.Fatalf("branch = %q, want stderr-probe", got)
	}
}

func TestInTimesOutHangingHook(t *testing.T) {
	orig := gitTimeout
	gitTimeout = 2 * time.Second
	t.Cleanup(func() { gitTimeout = orig })
	repo := Repo(t)
	hook := fmt.Sprintf("#!/bin/sh\nexec sleep %d\n", int(gitTimeout/time.Second)+15)
	if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "output")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	passed := func() bool {
		oldStdout := os.Stdout
		os.Stdout = output
		defer func() { os.Stdout = oldStdout }()
		return testing.RunTests(func(_, _ string) (bool, error) { return true, nil },
			[]testing.InternalTest{{Name: "hanging-hook", F: func(it *testing.T) {
				In(it, repo, "commit", "--allow-empty", "-m", "timeout-probe")
			}}})
	}()
	elapsed := time.Since(started)
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	buf, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if passed {
		t.Fatalf("In allowed the hanging pre-commit hook to finish instead of timing out: %s", buf)
	}
	if !strings.Contains(string(buf), "git [commit --allow-empty -m timeout-probe]:") {
		t.Fatalf("In failed without reporting the timed-out git command: %s", buf)
	}
	if elapsed > 2*gitTimeout+10*time.Second {
		t.Fatalf("In took %s, exceeding the context timeout and pipe-drain allowance", elapsed)
	}
}

func TestRepoAtCopyFailureNamesDestination(t *testing.T) {
	Repo(t) // runs once.Do so the tmpl swap below sticks
	oldTmpl := tmpl
	t.Cleanup(func() { tmpl = oldTmpl })
	tmpl = filepath.Join(t.TempDir(), "missing-template")
	dir := filepath.Join(t.TempDir(), "wanted-dest")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	testing.RunTests(func(_, _ string) (bool, error) { return true, nil },
		[]testing.InternalTest{{Name: "inner", F: func(it *testing.T) { RepoAt(it, dir) }}})
	os.Stdout = oldStdout
	_ = w.Close()
	buf, _ := io.ReadAll(r)

	if !strings.Contains(string(buf), dir) {
		t.Fatalf("copy failure message omits the destination %s: %q", dir, buf)
	}
}
