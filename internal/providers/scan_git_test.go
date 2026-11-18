package providers

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestScanGitRepoProbeFailuresAreNotClean(t *testing.T) {
	sleep, _ := exec.LookPath("sleep")
	for _, tt := range []struct {
		name   string
		script string
	}{
		{name: "missing executable"},
		{name: "timeout", script: "#!/bin/sh\nexec \"" + sleep + "\" 2\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.script != "" && (runtime.GOOS == "windows" || sleep == "") {
				t.Skip("the fake git is a POSIX shell script")
			}
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

func TestScanGitLogPlusPrefixedSecret(t *testing.T) {
	token := "ghp_" + "abcdefghij1234567890ABCD"
	name := "config-" + token + ".env"
	for _, content := range []string{"++TOKEN=", "++ TOKEN="} {
		t.Run(content, func(t *testing.T) {
			patch := "commit 0123456789abcdef\ndiff --git a/" + name + " b/" + name + "\n" +
				"--- /dev/null\n+++ b/" + name + "\n@@ -0,0 +1 @@\n+" + content + token + "\n"
			got, err := scanGitLogOutput(strings.NewReader(patch), nil)
			want := []string{"0123456789ab " + name}
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("scan plus-prefixed addition = %v, %v; want %v", got, err, want)
			}
		})
	}
}

func TestScanGitLogSecretStraddlesFragment(t *testing.T) {
	token := "ghp_" + "abcdefghij1234567890ABCD"
	line := "+" + strings.Repeat("x", (1<<20)-3) + token + "\n"
	patch := "commit 0123456789abcdef\ndiff --git a/config.env b/config.env\n" +
		"--- /dev/null\n+++ b/config.env\n@@ -0,0 +1 @@\n" + line
	got, err := scanGitLogOutput(strings.NewReader(patch), nil)
	want := []string{"0123456789ab config.env"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("scan token split after gh = %v, %v; want %v", got, err, want)
	}
}

func TestIsTestFixture(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"internal/vault/git_test.go", true},
		{"git_test.go", true},
		{"internal/vault/testdata/keys.env", true},
		{"testdata/x.json", true},
		{"scripts/scan_test.py", true},
		{"web/src/app.test.ts", true},
		{"web/src/app.test.js", true},
		{"web/src/app.spec.ts", true},
		{"web/src/app.spec.js", true},
		{"internal/vault/git.go", false},
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
	run := gitIn(t, dir)
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
	if err := os.WriteFile(filepath.Join(dir, "é.go"), []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "x_test.go", "testdata/keys.env", "é.go")
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
	if !strings.Contains(out, "é.go") {
		t.Errorf("output does not name the non-ASCII file:\n%s", out)
	}
	if !strings.Contains(out, "2 finding(s)") {
		t.Errorf("want exactly 2 findings:\n%s", out)
	}
}

// ScanGit must finish promptly and find a planted secret when another history line is enormous.
func TestScanGitLongLine(t *testing.T) {
	for _, order := range []string{"leak-then-long", "long-then-leak"} {
		t.Run(order, func(t *testing.T) {
			dir := hosttest.Repo(t)
			run := gitIn(t, dir)
			long := strings.Repeat("x", 1<<20+1)
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
			checkLongLineScan(t, dir)
		})
	}
}

// gitIn returns a runner for git subcommands in dir that fails the test on any error.
func gitIn(t *testing.T, dir string) func(args ...string) {
	return func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// checkLongLineScan asserts ScanGit finishes under 5s and names leak.txt with no timeout or truncation.
func checkLongLineScan(t *testing.T, dir string) {
	t.Helper()
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
}

// A secret on a fragment of an over-long added line must be reported exactly once, not once per fragment.
func TestScanGitSecretInsideLongLine(t *testing.T) {
	dir := runGitScratch(t)
	run := gitIn(t, dir)
	line := strings.Repeat("x", 15*(1<<17)) + "ghp_abcdefghij1234567890ABCD"
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

// ScanGit passes --no-textconv so a repo-configured textconv filter never executes during the scan.
func TestScanGitNoTextconv(t *testing.T) {
	dir := runGitScratch(t)
	run := gitIn(t, dir)
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

func TestScanGitRejectsOptionLikeRev(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	var out bytes.Buffer
	if code := ScanGit(&out, dir, "--output="+victim, 0); code != ExitFindings {
		t.Fatalf("exit = %d, want %d: %s", code, ExitFindings, out.String())
	}
	if _, err := os.Stat(victim); err == nil {
		t.Errorf("git wrote %s: rev was parsed as an option", victim)
	}
	if !strings.Contains(out.String(), "invalid rev") {
		t.Errorf("output = %q, want invalid rev notice", out.String())
	}
}

// runGitScratch initializes a throwaway repo with a committed secret-shaped line, for ScanGit tests.
func runGitScratch(t *testing.T) string {
	dir := hosttest.Repo(t)
	path := filepath.Join(dir, "config.env")
	if err := os.WriteFile(path, []byte("GITHUB_TOKEN=ghp_0123456789abcdefghij\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, dir, "add", "config.env")
	hosttest.In(t, dir, "commit", "-q", "-m", "add config")
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
	dir := hosttest.Repo(t)
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, dir, "add", "readme.md")
	hosttest.In(t, dir, "commit", "-q", "-m", "init")

	var buf bytes.Buffer
	if rc := ScanGit(&buf, dir, "", 0); rc != ExitClean {
		t.Fatalf("ScanGit rc = %d, want %d:\n%s", rc, ExitClean, buf.String())
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

// secretPattern (raw-text scan) catches every GitHub family at 20+ body characters and not shorter.
func TestSecretPatternGitHubFamilies(t *testing.T) {
	for _, p := range []string{"gho_", "ghp_", "ghu_", "ghs_", "ghr_", "github_pat_"} {
		if !secretPattern().MatchString("x = " + p + strings.Repeat("a", 20)) {
			t.Errorf("%s with a 20-char body must be caught", p)
		}
		if secretPattern().MatchString("x = " + p + strings.Repeat("a", 19)) {
			t.Errorf("%s with a 19-char body must not be caught", p)
		}
	}
}

func addedPatch(file, line string) string {
	return "commit 0123456789abcdef\ndiff --git a/" + file + " b/" + file + "\n--- /dev/null\n+++ b/" + file +
		"\n@@ -0,0 +1,2 @@\n+first\n+" + line + "\n"
}

// A real-shaped host, a workspace id and a configured-profile host are flagged by kind and line, never by value.
func TestScanGitFlagsWorkspaceIdentifiers(t *testing.T) {
	deny := []string{"corp-private.example.net", "dev.corp-fake.cloud.databricks.com"}
	const adb = "adb-1234567890123456.7.azuredatabricks.net"
	cases := map[string]struct{ line, kind string }{
		"azure":   {"url: https://" + adb, "real-shaped"},
		"upper":   {"url: HTTPS://" + strings.ToUpper(adb), "real-shaped"},
		"urlenc":  {"url: https%3A%2F%2F" + adb, "real-shaped"},
		"newline": {`{"a":"x\n` + adb + `"}`, "real-shaped"},
		"slash":   {`{"a":"https:\/\/` + adb + `"}`, "real-shaped"},
		"label":   {"host: foo." + adb, "real-shaped"},
		"cloud":   {"host: acme.cloud.databricks.com", "real-shaped"},
		"dbc":     {"host: dbc-1a2b3c4d-5e6f.cloud.databricks.com", "real-shaped"},
		"gcp":     {"host: acme.gcp.databricks.com", "real-shaped"},
		"orgid":   {"https://x.test/databricks/?o=1234567890123456", "workspace id"},
		"denied":  {"see Corp-Private.example.net", "configured Databricks profile"},
		"devhost": {"host: dev.corp-fake.cloud.databricks.com", "configured Databricks profile"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := scanGitLogOutput(strings.NewReader(addedPatch("doc.md", c.line)), deny)
			if err != nil || len(got) < 1 || !strings.Contains(got[0], "doc.md:2 — ") || !strings.Contains(got[0], c.kind) {
				t.Fatalf("findings = %v, %v; want a %q finding at doc.md:2", got, err, c.kind)
			}
			if strings.Contains(strings.ToLower(got[0]), "acme") || strings.Contains(got[0], "1234567890") {
				t.Errorf("finding echoes the value: %v", got)
			}
		})
	}
}

// The repo's documented placeholders never count; fixtures skip the shape rules but not the configured-host list.
func TestScanGitAllowsPlaceholders(t *testing.T) {
	for _, line := range []string{
		"host: example.cloud.databricks.com", "host: dev.example.cloud.databricks.com",
		"host: prod.cloud.databricks.com", "host: adb-1.azuredatabricks.net", "host: dbc-1.cloud.databricks.com",
	} {
		if got, _ := scanGitLogOutput(strings.NewReader(addedPatch("doc.md", line)), nil); len(got) != 0 {
			t.Errorf("%q flagged: %v", line, got)
		}
	}
	fake := "host: adb-1234567890123456.7.azuredatabricks.net"
	if got, _ := scanGitLogOutput(strings.NewReader(addedPatch("a_test.go", fake)), nil); len(got) != 0 {
		t.Errorf("test fixture shape flagged: %v", got)
	}
	deny := []string{"corp-private.example.net"}
	if got, _ := scanGitLogOutput(strings.NewReader(addedPatch("a_test.go", "see corp-private.example.net")), deny); len(got) != 1 {
		t.Errorf("configured host in a fixture not flagged: %v", got)
	}
}

// An unreadable registry is announced without echoing its error's host, and the shape rules still decide the exit.
func TestScanGitRegistryUnreadable(t *testing.T) {
	old := denyHosts
	denyHosts = func() ([]string, error) { return nil, errors.New("overlay: bad row for acme.cloud.databricks.com") }
	t.Cleanup(func() { denyHosts = old })
	dir := hosttest.Repo(t)
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("host: acme.cloud.databricks.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hosttest.In(t, dir, "add", "n.md")
	hosttest.In(t, dir, "commit", "-q", "-m", "add")
	var buf bytes.Buffer
	rc := ScanGit(&buf, dir, "", 0)
	if out := buf.String(); rc != ExitFindings || !strings.Contains(out, "check disabled") || strings.Contains(out, "acme") {
		t.Fatalf("rc = %d, want findings plus the notice without the host:\n%s", rc, out)
	}
}

// Every configured host stays on the deny list, whatever its first label; only example hosts drop out.
func TestDenyHost(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Dev.corp-fake.cloud.databricks.com": "dev.corp-fake.cloud.databricks.com",
		"https://x.corp-fake.cloud.databricks.com":   "x.corp-fake.cloud.databricks.com",
		"https://dev.example.cloud.databricks.com":   "",
		"https://localhost:8080":                     "",
	} {
		if got := denyHost(raw); got != want {
			t.Errorf("denyHost(%q) = %q, want %q", raw, got, want)
		}
	}
}
