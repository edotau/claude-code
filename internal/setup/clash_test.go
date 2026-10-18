package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

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

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestClashes(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo, map[string]string{
		"CLAUDE.md":   "repo claude\n",
		"agents/a.md": "agent a\n",
		"rules/r.md":  "rule r\n",
	})
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CLAUDE.md"), "user claude\n")
	mustWrite(t, filepath.Join(dir, "agents", "a.md"), "agent a\n")
	mustWrite(t, filepath.Join(dir, "skills", "mine", "SKILL.md"), "mine\n")

	clashes, err := Clashes(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(clashes) != 2 {
		t.Fatalf("want 2 clashes, got %d: %+v", len(clashes), clashes)
	}
	byPath := map[string]Clash{}
	for _, c := range clashes {
		byPath[c.Path] = c
	}
	if c, ok := byPath["CLAUDE.md"]; !ok || !c.Differs {
		t.Error("CLAUDE.md should be a clash that differs")
	}
	if c, ok := byPath["agents/a.md"]; !ok || c.Differs {
		t.Error("agents/a.md should be a clash with the same content")
	}
	if _, ok := byPath["rules/r.md"]; ok {
		t.Error("rules/r.md is not present at dir, so it is not a clash")
	}
}

func TestClashesUntrackedNeverClash(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo, map[string]string{"CLAUDE.md": "repo claude\n"})
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "skills", "mine", "SKILL.md"), "mine\n")
	mustWrite(t, filepath.Join(dir, "settings.local.json"), "{}\n")

	clashes, err := Clashes(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(clashes) != 0 {
		t.Fatalf("want 0 clashes, got %+v", clashes)
	}
}
