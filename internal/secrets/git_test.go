package secrets

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsTestFixture(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"internal/secrets/git_test.go", true},
		{"git_test.go", true},
		{"internal/secrets/testdata/keys.env", true},
		{"testdata/x.json", true},
		{"scripts/scan_test.py", true},
		{"web/src/app.test.ts", true},
		{"web/src/app.test.js", true},
		{"web/src/app.spec.ts", true},
		{"web/src/app.spec.js", true},
		{"internal/secrets/git.go", false},
		{"config.env", false},
		{"mytestdata/keys.env", false},
		{"testdata.go", false},
		{"internal/test.go", false},
		{"app.test.tsx", false},
	}
	for _, c := range cases {
		if got := isTestFixture(c.path); got != c.want {
			t.Errorf("isTestFixture(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// ScanGit must skip secret-shaped additions in test fixtures but still report real files.
func TestScanGitSkipsTestFixtures(t *testing.T) {
	dir := runGitScratch(t)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	// Fixtures are assembled at runtime so push protection never sees a literal key in this file.
	fixture := "package x\n\nconst tok = \"sk" + "-abcdefghijklmnopqrstuvwxyz0123\"\n"
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "testdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, "testdata", "keys.env")
	if err := os.WriteFile(keys, []byte("K=ghp"+"_abcdefghij0123456789\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "x_test.go", "testdata/keys.env")
	run("commit", "-q", "-m", "add fixtures")

	var buf bytes.Buffer
	if rc := ScanGit(&buf, dir, 0); rc != ExitFindings {
		t.Fatalf("ScanGit rc = %d, want %d:\n%s", rc, ExitFindings, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "config.env") {
		t.Errorf("output does not name the non-test file:\n%s", out)
	}
	if strings.Contains(out, "x_test.go") || strings.Contains(out, "testdata/") {
		t.Errorf("output flags a test fixture:\n%s", out)
	}
	if !strings.Contains(out, "1 finding(s)") {
		t.Errorf("want exactly 1 finding:\n%s", out)
	}
}

// Prose that only starts like a key carries no digits; a minted token always does.
func TestTokenShaped(t *testing.T) {
	jwt := "eyJ" + strings.Repeat("a", 12) + "." + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12)
	cases := map[string]bool{"": false, "sk-before-changing-appearance-here": false, "sk" + "-abcdefghijklmnopqrstuvwxyz0123": true, jwt: true}
	for in, want := range cases {
		if got := tokenShaped(in); got != want {
			t.Errorf("tokenShaped(%q) = %v, want %v", in, got, want)
		}
	}
}
