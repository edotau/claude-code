package setup

import (
	"path/filepath"
	"testing"
)

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
