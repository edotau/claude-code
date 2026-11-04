package vault

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLooksLikeSecret(t *testing.T) {
	cases := map[string]bool{
		"sk-abcdefghij0123456789":    true,
		"ghp_abcdefghij0123456789":   true,
		"${ANTHROPIC_API_KEY}":       false,
		"$ANTHROPIC_API_KEY":         false,
		"/home/user/.ssh/token-file": false,
		"":                           false,
		"YOUR_API_KEY_HERE":          false,
	}
	for v, want := range cases {
		if got := LooksLikeSecret(v); got != want {
			t.Errorf("LooksLikeSecret(%q) = %v, want %v", v, got, want)
		}
	}
}

// ScanFile must find a planted sk-... value in a JSON config and never flag a ${ENV} reference.
// runGitScratch initializes a throwaway repo with a committed secret-shaped line, for ScanGit tests.
func runGitScratch(t *testing.T) string {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	path := filepath.Join(dir, "config.env")
	if err := os.WriteFile(path, []byte("GITHUB_TOKEN=ghp_0123456789abcdefghij\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "config.env")
	run("commit", "-q", "-m", "add config")
	return dir
}

// ScanGit must find a secret-shaped line committed to history, without ever printing it.
func TestScanGitFindsPlantedSecret(t *testing.T) {
	dir := runGitScratch(t)
	var buf bytes.Buffer
	rc := ScanGit(&buf, dir, "", 0)
	if rc != ExitFindings {
		t.Fatalf("ScanGit rc = %d, want %d:\n%s", rc, ExitFindings, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "config.env") {
		t.Errorf("output does not name the file:\n%s", out)
	}
	if strings.Contains(out, "ghp_0123456789abcdefghij") {
		t.Fatal("git scan output must never echo the matched secret")
	}
}

// A repo with no secret-shaped history must report clean.
func TestScanGitCleanHistory(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	os.WriteFile(filepath.Join(dir, "readme.md"), []byte("hello\n"), 0o644)
	run("add", "readme.md")
	run("commit", "-q", "-m", "init")

	var buf bytes.Buffer
	if rc := ScanGit(&buf, dir, "", 0); rc != ExitClean {
		t.Fatalf("ScanGit rc = %d, want %d:\n%s", rc, ExitClean, buf.String())
	}
}

// ScanGit outside any git repo must report clean (skipped), not an error.
func TestScanGitNotARepo(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if rc := ScanGit(&buf, dir, "", 0); rc != ExitClean {
		t.Fatalf("ScanGit rc = %d, want %d (skipped):\n%s", rc, ExitClean, buf.String())
	}
}

// secretPattern (raw-text scan) catches every GitHub family at 20+ body characters and not shorter.
func TestSecretPatternGitHubFamilies(t *testing.T) {
	for _, p := range []string{"gho_", "ghp_", "ghu_", "ghs_", "ghr_", "github_pat_"} {
		if !secretPattern.MatchString("x = " + p + strings.Repeat("a", 20)) {
			t.Errorf("%s with a 20-char body must be caught", p)
		}
		if secretPattern.MatchString("x = " + p + strings.Repeat("a", 19)) {
			t.Errorf("%s with a 19-char body must not be caught", p)
		}
	}
}
