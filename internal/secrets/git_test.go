package secrets

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestScanGitRepoProbeFailuresAreNotClean(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		script string
	}{
		{name: "missing executable"},
		{name: "timeout", script: "#!/bin/sh\nexec \"" + sleep + "\" 2\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bin := t.TempDir()
			if tt.script != "" {
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(tt.script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)
			oldTimeout := gitLogTimeout
			gitLogTimeout = 50 * time.Millisecond
			t.Cleanup(func() { gitLogTimeout = oldTimeout })

			var out bytes.Buffer
			started := time.Now()
			if code := ScanGit(&out, t.TempDir(), "", 0); code != ExitFindings {
				t.Errorf("ScanGit failed probe exit = %d, want %d; output = %q", code, ExitFindings, out.String())
			}
			if elapsed := time.Since(started); elapsed >= time.Second {
				t.Errorf("repo probe took %v with timeout %v", elapsed, gitLogTimeout)
			}
			if strings.Contains(out.String(), "skipped") || strings.Contains(out.String(), "clean.") {
				t.Errorf("failed probe reported clean/skip: %q", out.String())
			}
		})
	}
}

func TestScanGitNonRepoStillSkipped(t *testing.T) {
	var out bytes.Buffer
	if code := ScanGit(&out, t.TempDir(), "", 0); code != ExitClean {
		t.Fatalf("non-repo exit = %d, want %d: %s", code, ExitClean, out.String())
	}
	if !strings.Contains(out.String(), "not inside a git repo") || !strings.Contains(out.String(), "skipped") {
		t.Errorf("non-repo output = %q, want skip notice", out.String())
	}
}

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
	if rc := ScanGit(&buf, dir, "", 0); rc != ExitFindings {
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

// ScanGit must still finish promptly and find a planted secret when some other line in history is
// enormous (a minified bundle or lockfile) — ReadLine fragments it instead of failing the scan.
func TestScanGitLongLine(t *testing.T) {
	for _, order := range []string{"leak-then-long", "long-then-leak"} {
		t.Run(order, func(t *testing.T) {
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
			long := strings.Repeat("x", 2<<20) // 2 MiB, one line
			leak := "k=ghp_abcdefghij1234567890ABCD\n"

			writeCommit := func(name, content string) {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				run("add", name)
				run("commit", "-q", "-m", "add "+name)
			}
			if order == "leak-then-long" {
				writeCommit("leak.txt", leak)
				writeCommit("big.txt", long+"\n")
			} else {
				writeCommit("big.txt", long+"\n")
				writeCommit("leak.txt", leak)
			}

			var buf bytes.Buffer
			started := time.Now()
			rc := ScanGit(&buf, dir, "", 0)
			if elapsed := time.Since(started); elapsed >= 5*time.Second {
				t.Errorf("ScanGit took %v, want < 5s", elapsed)
			}
			if rc != ExitFindings {
				t.Fatalf("ScanGit rc = %d, want %d:\n%s", rc, ExitFindings, buf.String())
			}
			out := buf.String()
			if !strings.Contains(out, "leak.txt") {
				t.Errorf("output does not name leak.txt:\n%s", out)
			}
			if strings.Contains(out, "timed out") || strings.Contains(out, "truncated") {
				t.Errorf("output reports a timeout/truncation:\n%s", out)
			}
		})
	}
}

// A secret that lands on a fragment of an over-long added line must still be reported exactly once,
// not once per ReadLine fragment.
func TestScanGitSecretInsideLongLine(t *testing.T) {
	dir := runGitScratch(t) // seeds an initial unrelated commit so this isn't the repo's first commit
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	line := strings.Repeat("x", 15*(1<<17)) + "ghp_abcdefghij1234567890ABCD" // ~1.5 MiB prefix + token
	if err := os.WriteFile(filepath.Join(dir, "long.txt"), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "long.txt")
	run("commit", "-q", "-m", "add long.txt")

	var buf bytes.Buffer
	rc := ScanGit(&buf, dir, "", 0)
	if rc != ExitFindings {
		t.Fatalf("ScanGit rc = %d, want %d:\n%s", rc, ExitFindings, buf.String())
	}
	out := buf.String()
	if n := strings.Count(out, "long.txt"); n != 1 {
		t.Errorf("long.txt reported %d time(s), want exactly 1:\n%s", n, out)
	}
}

// ScanGit passes --no-textconv, so a repo-configured textconv filter (which could run arbitrary
// commands) must never execute during the scan.
func TestScanGitNoTextconv(t *testing.T) {
	dir := runGitScratch(t)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	marker := filepath.Join(dir, "MARKER")
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt diff=t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("config", "diff.t.textconv", "sh -c 'touch "+marker+"; cat'")
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".gitattributes", "plain.txt")
	run("commit", "-q", "-m", "add textconv config")

	var buf bytes.Buffer
	ScanGit(&buf, dir, "", 0)
	if _, err := os.Stat(marker); err == nil {
		t.Error("textconv filter ran during ScanGit (MARKER present)")
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
