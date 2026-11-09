package vault

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest/gittest"
)

func TestMasterKeyStatusRejectsCorruptSecrets(t *testing.T) {
	for _, malformed := range []bool{true, false} {
		t.Run(fmt.Sprintf("malformed-json=%t", malformed), func(t *testing.T) {
			phase1bRing(t)
			phase1bPut(t, "A_GOOD", "healthy-value")
			phase1bPut(t, "Z_BAD", "damaged-value")
			if count, source, err := MasterKeyStatus(); count != 2 || source != "keyring" || err != nil {
				t.Fatalf("healthy status = %d, %q, %v", count, source, err)
			}
			corruptHealthSecret(t, malformed)
			count, source, err := MasterKeyStatus()
			if count != 2 || source != "keyring" || err == nil {
				t.Fatalf("corrupt status = %d, %q, %v; want 2, keyring, error", count, source, err)
			}
		})
	}
}

func corruptHealthSecret(t *testing.T, malformed bool) {
	t.Helper()
	if err := Update(func(tx *Tx) error {
		if malformed {
			return tx.Put(Secrets, "Z_BAD", "{")
		}
		r, _, err := tx.Record("Z_BAD")
		if err != nil {
			return err
		}
		r.Sealed[len(r.Sealed)-1] ^= 1
		return tx.PutRecord("Z_BAD", r)
	}); err != nil {
		t.Fatal(err)
	}
}

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
	dir := gittest.Repo(t)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
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
	dir := gittest.Repo(t)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
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
