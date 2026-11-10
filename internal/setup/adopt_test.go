package setup

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

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

// Adopt refuses an Into that is already a checkout with ErrAlreadyCheckout, before touching either dir.
func TestAdoptRefusesExistingGit(t *testing.T) {
	repo := fixtureRepo(t)
	dir := fixtureTarget(t)
	mustWrite(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")
	before := snapshot(t, dir)
	if err := Adopt(AdoptOpts{From: repo, Into: dir}); !errors.Is(err, ErrAlreadyCheckout) {
		t.Fatalf("Adopt into a checkout = %v, want ErrAlreadyCheckout", err)
	}
	if !reflect.DeepEqual(before, snapshot(t, dir)) {
		t.Error("refused adopt changed the target dir")
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Errorf("refused adopt moved the source .git: %v", err)
	}
}

// Adopt refuses a From without .git with ErrNotAClone, before touching the target.
func TestAdoptRefusesNonCloneFrom(t *testing.T) {
	dir := fixtureTarget(t)
	before := snapshot(t, dir)
	if err := Adopt(AdoptOpts{From: t.TempDir(), Into: dir}); !errors.Is(err, ErrNotAClone) {
		t.Fatalf("Adopt from a non-clone = %v, want ErrNotAClone", err)
	}
	if !reflect.DeepEqual(before, snapshot(t, dir)) {
		t.Error("refused adopt changed the target dir")
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

	assertTrackedRestored(t, dir)
	assertUserStateSurvives(t, dir)
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
	assertBackupHolds(t, st.BackupDir, map[string]bool{"CLAUDE.md": true, "agents/a.md": true})
}

// assertTrackedRestored checks the repo's tracked files now hold the repo content in dir.
func assertTrackedRestored(t *testing.T, dir string) {
	t.Helper()
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
}

// assertUserStateSurvives checks the untracked skill and the runtime state dirs are still in dir.
func assertUserStateSurvives(t *testing.T, dir string) {
	t.Helper()
	survivors := []struct{ rel, what string }{
		{filepath.Join("skills", "mine", "SKILL.md"), "untracked skill should survive adopt"},
		{filepath.Join("projects", "p1", "transcript.jsonl"), "projects/ runtime state should survive adopt"},
		{filepath.Join("sessions", "s1.json"), "sessions/ runtime state should survive adopt"},
	}
	for _, sv := range survivors {
		if _, err := os.Stat(filepath.Join(dir, sv.rel)); err != nil {
			t.Fatal(sv.what)
		}
	}
}

// assertBackupHolds checks backupDir holds exactly the want entries and never the vault or legacy secrets.
func assertBackupHolds(t *testing.T, backupDir string, want map[string]bool) {
	t.Helper()
	entries := collectFiles(t, backupDir)
	if len(entries) != len(want) {
		t.Fatalf("backup has %v, want exactly %v", entries, want)
	}
	for _, e := range entries {
		if !want[e] {
			t.Fatalf("unexpected backup entry %q", e)
		}
	}
	for _, secretish := range entries {
		if secretish == "state/vault.db" || secretish == "env.d/secrets.env" {
			t.Fatal("vault and legacy secrets must never be backed up as clashes")
		}
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

func TestClashesDanglingSymlinkIsAClash(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo, map[string]string{"CLAUDE.md": "repo claude\n"})
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	clashes, err := Clashes(repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(clashes) != 1 || clashes[0].Path != "CLAUDE.md" {
		t.Fatalf("dangling symlink must be a clash, got %+v", clashes)
	}
}

func TestClashesStatErrorIsReturned(t *testing.T) {
	repo := t.TempDir()
	gitInit(t, repo, map[string]string{"agents/a.md": "a\n"})
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "agents"), "a file, not a dir\n") // Lstat agents/a.md -> ENOTDIR
	if _, err := Clashes(repo, dir); err == nil {
		t.Fatal("non-NotExist stat error was swallowed")
	}
}

func TestMoveGitAndCheckoutRollsBackGit(t *testing.T) {
	from := t.TempDir()
	gitInit(t, from, map[string]string{"agents/a.md": "a\n"})
	into := t.TempDir()
	mustWrite(t, filepath.Join(from, ".git", "index"), "corrupt\n") // makes git checkout fail
	if err := moveGitAndCheckout(from, into); err == nil {
		t.Fatal("checkout should have failed")
	}
	if _, err := os.Stat(filepath.Join(into, ".git")); err == nil {
		t.Error(".git left in Into after failed checkout")
	}
	if _, err := os.Stat(filepath.Join(from, ".git")); err != nil {
		t.Errorf(".git not restored to From: %v", err)
	}
}

// A dangling symlink at the target is backed up as a link instead of aborting Backup on os.Stat.
func TestBackupDanglingSymlink(t *testing.T) {
	dir := vaulttest.Use(t)
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	dest, err := Backup(dir, []string{"CLAUDE.md"}, "adopt", time.Now())
	if err != nil {
		t.Fatalf("Backup = %v, want the dangling link copied", err)
	}
	if got, err := os.Readlink(filepath.Join(dest, "CLAUDE.md")); err != nil || got != filepath.Join(dir, "nowhere") {
		t.Fatalf("backup link = %q, %v", got, err)
	}
}

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
