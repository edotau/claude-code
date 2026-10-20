package setup

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func fixtureRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitInit(t, repo, map[string]string{
		"CLAUDE.md":   "repo claude\n",
		"agents/a.md": "agent a\n",
		"rules/r.md":  "rule r\n",
	})
	return repo
}

func fixtureTarget(t *testing.T) string {
	t.Helper()
	dir := vaulttest.Use(t)
	mustWrite(t, filepath.Join(dir, "CLAUDE.md"), "user claude\n")
	mustWrite(t, filepath.Join(dir, "agents", "a.md"), "agent a\n")
	mustWrite(t, filepath.Join(dir, "skills", "mine", "SKILL.md"), "mine\n")
	vaulttest.Secret(t, "API_KEY", "test-value")
	mustWrite(t, filepath.Join(dir, "projects", "p1", "transcript.jsonl"), "{}\n")
	mustWrite(t, filepath.Join(dir, "sessions", "s1.json"), "{}\n")
	return dir
}

// snapshot reads every file under dir into a relpath -> content map, for before/after comparison.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		data, _ := os.ReadFile(path)
		out[rel] = string(data)
		return nil
	})
	return out
}

func collectFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out
}

func TestAdoptDryRunWritesNothing(t *testing.T) {
	repo := fixtureRepo(t)
	dir := fixtureTarget(t)
	before := snapshot(t, dir)

	var out bytes.Buffer
	if err := Adopt(AdoptOpts{From: repo, Into: dir, Label: "adopt", DryRun: true, Out: &out}); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, dir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("dry-run changed the target dir:\nbefore=%v\nafter=%v", before, after)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatal("source .git should be untouched by a dry run")
	}
	if out.Len() == 0 {
		t.Fatal("dry-run should still print the clash report")
	}
}

func TestAdoptRefusesExistingGit(t *testing.T) {
	repo := fixtureRepo(t)
	dir := fixtureTarget(t)
	mustWrite(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")

	if err := Adopt(AdoptOpts{From: repo, Into: dir}); err == nil {
		t.Fatal("want a refusal when Into/.git already exists")
	}
}

func TestAdoptRefusesNonCloneFrom(t *testing.T) {
	dir := fixtureTarget(t)
	notAClone := t.TempDir()

	if err := Adopt(AdoptOpts{From: notAClone, Into: dir}); err == nil {
		t.Fatal("want a refusal when From/.git is missing")
	}
}

func TestAdoptBacksUpClashesAndLeavesRestAlone(t *testing.T) {
	repo := fixtureRepo(t)
	dir := fixtureTarget(t)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	vaultBefore, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Adopt(AdoptOpts{From: repo, Into: dir, Label: "adopt", Out: &out}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(repo); !os.IsNotExist(err) {
		t.Fatalf("From dir should be removed, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatal("Into should now be a git checkout")
	}

	want := map[string]string{"CLAUDE.md": "repo claude\n", "agents/a.md": "agent a\n", "rules/r.md": "rule r\n"}
	for rel, content := range want {
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if string(got) != content {
			t.Fatalf("%s = %q, want %q", rel, got, content)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "skills", "mine", "SKILL.md")); err != nil {
		t.Fatal("untracked skill should survive adopt")
	}
	if _, err := os.Stat(filepath.Join(dir, "projects", "p1", "transcript.jsonl")); err != nil {
		t.Fatal("projects/ runtime state should survive adopt")
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions", "s1.json")); err != nil {
		t.Fatal("sessions/ runtime state should survive adopt")
	}
	vaultAfter, err := os.ReadFile(vault.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(vaultBefore, vaultAfter) {
		t.Fatal("adopt must leave the vault unchanged")
	}

	st, ok := Read()
	if !ok {
		t.Fatal("expected a recorded setup state")
	}
	if st.ClashCount != 2 {
		t.Fatalf("ClashCount = %d, want 2", st.ClashCount)
	}
	entries := collectFiles(t, st.BackupDir)
	wantBackup := map[string]bool{"CLAUDE.md": true, "agents/a.md": true}
	if len(entries) != len(wantBackup) {
		t.Fatalf("backup has %v, want exactly %v", entries, wantBackup)
	}
	for _, e := range entries {
		if !wantBackup[e] {
			t.Fatalf("unexpected backup entry %q", e)
		}
	}
	for _, secretish := range entries {
		if secretish == "state/vault.db" || secretish == "env.d/secrets.env" {
			t.Fatal("vault and legacy secrets must never be backed up as clashes")
		}
	}
}
