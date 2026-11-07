package gittest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
