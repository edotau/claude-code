package secrets

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func setConfigDir(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

// noKeyring points PATH at an empty dir so keyring.Available() is false and Store/Delete never
// touch the real OS keychain.
func noKeyring(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// Store with forceFile must always write the file plane, even though no keyring is on PATH anyway.
func TestStoreForceFileWritesFile(t *testing.T) {
	setConfigDir(t)
	plane, err := Store("A_KEY", "v", true)
	if err != nil {
		t.Fatal(err)
	}
	if plane != EnvFile() {
		t.Errorf("plane = %q, want %q", plane, EnvFile())
	}
	if got := ReadAll()["A_KEY"]; got != "v" {
		t.Errorf("A_KEY = %q, want v", got)
	}
}

// Store with no keyring tool available must fall back to the file plane, not error.
func TestStoreFallsBackToFileWithoutKeyring(t *testing.T) {
	setConfigDir(t)
	noKeyring(t)
	plane, err := Store("B_KEY", "v", false)
	if err != nil {
		t.Fatal(err)
	}
	if plane != EnvFile() {
		t.Errorf("plane = %q, want %q", plane, EnvFile())
	}
	if got := ReadAll()["B_KEY"]; got != "v" {
		t.Errorf("B_KEY = %q, want v", got)
	}
}

// Delete must remove the name from the file plane (the keyring side is a no-op without a tool).
func TestDeleteRemovesFileLine(t *testing.T) {
	setConfigDir(t)
	noKeyring(t)
	if err := SetFile("C_KEY", "v"); err != nil {
		t.Fatal(err)
	}
	if err := Delete("C_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadAll()["C_KEY"]; ok {
		t.Error("C_KEY still present after Delete")
	}
}

// Delete on a name stored nowhere must be a no-op, not an error.
func TestDeleteMissingIsNoop(t *testing.T) {
	setConfigDir(t)
	noKeyring(t)
	if err := Delete("NEVER_SET"); err != nil {
		t.Fatalf("Delete on an unset name must not error: %v", err)
	}
}

// Redact must show only the length for a short value, and never the middle of a long one.
func TestRedact(t *testing.T) {
	if got := Redact("short"); got != "5 chars" {
		t.Errorf("Redact(short) = %q, want %q", got, "5 chars")
	}
	if got := Redact("sk-abcdefghijklmnopq"); got != "20 chars" {
		t.Errorf("Redact(20 chars) = %q, want length only", got)
	}
	long := "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"
	got := Redact(long)
	want := long[:8] + "…" + long[len(long)-4:] + ", " + strconv.Itoa(len(long)) + " chars"
	if got != want {
		t.Errorf("Redact(long) = %q, want %q", got, want)
	}
	if strings.Contains(got, long[9:len(long)-5]) {
		t.Errorf("Redact(long) leaked the middle of the value: %q", got)
	}
}

// SetFile/Get must round-trip through the file plane, and the written file must be 0600.
func TestSetFileGetRoundTripAndPerms(t *testing.T) {
	setConfigDir(t)
	if err := SetFile("OPENROUTER_API_KEY", "sk-test-abc123"); err != nil {
		t.Fatalf("SetFile: %v", err)
	}
	fi, err := os.Stat(EnvFile())
	if err != nil {
		t.Fatalf("stat %s: %v", EnvFile(), err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %04o, want 0600", perm)
	}
	val, source, ok := Get("OPENROUTER_API_KEY")
	if !ok || val != "sk-test-abc123" || source != "file" {
		t.Fatalf("Get = (%q, %q, %v), want (sk-test-abc123, file, true)", val, source, ok)
	}
}

// A later SetFile must replace the value, not duplicate the key or disturb an unrelated one.
func TestSetFileReplacesAndKeepsOthers(t *testing.T) {
	setConfigDir(t)
	if err := SetFile("A_KEY", "first"); err != nil {
		t.Fatal(err)
	}
	if err := SetFile("B_KEY", "untouched"); err != nil {
		t.Fatal(err)
	}
	if err := SetFile("A_KEY", "second"); err != nil {
		t.Fatal(err)
	}
	got := ReadAll()
	if got["A_KEY"] != "second" {
		t.Errorf("A_KEY = %q, want second", got["A_KEY"])
	}
	if got["B_KEY"] != "untouched" {
		t.Errorf("B_KEY = %q, want untouched", got["B_KEY"])
	}
}

// A process env export must win over the file plane per the keyring -> file -> env ladder reversed:
// env is the last resort, consulted only when neither keyring nor file has the name.
func TestGetFallsThroughToEnvOnlyWhenFileEmpty(t *testing.T) {
	setConfigDir(t)
	t.Setenv("ONLY_IN_ENV", "from-process-env")
	val, source, ok := Get("ONLY_IN_ENV")
	if !ok || val != "from-process-env" || source != "env" {
		t.Fatalf("Get = (%q, %q, %v), want (from-process-env, env, true)", val, source, ok)
	}
}

// Get on a name set nowhere must miss cleanly, never an error or a panic.
func TestGetMiss(t *testing.T) {
	setConfigDir(t)
	if _, _, ok := Get("NEVER_SET"); ok {
		t.Fatal("Get on an unset name must report ok=false")
	}
}

// LooksLikeSecret must flag a real-shaped token and ignore an ${ENV} reference.
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
func TestScanFileFindsPlantedSecretAndIgnoresEnvRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	doc := `{"env": {"ANTHROPIC_API_KEY": "${ANTHROPIC_API_KEY}", "LEAKED": "sk-abcdefghij0123456789"}}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	s := &Scanner{Out: &buf}
	s.ScanFile(path)
	if len(s.Findings) != 1 {
		t.Fatalf("Findings = %v, want exactly 1 (the LEAKED key, not the ${ENV} ref)", s.Findings)
	}
	if !strings.Contains(s.Findings[0], "LEAKED") {
		t.Errorf("finding %q does not name the LEAKED key", s.Findings[0])
	}
	if strings.Contains(buf.String(), "sk-abcdefghij0123456789") {
		t.Fatal("scan output must never echo the secret value itself")
	}
}

// A clean config file must report ok with no findings.
func TestScanFileClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, []byte(`{"env": {"ANTHROPIC_API_KEY": "${ANTHROPIC_API_KEY}"}}`), 0o644)
	var buf bytes.Buffer
	s := &Scanner{Out: &buf}
	s.ScanFile(path)
	if len(s.Findings) != 0 {
		t.Fatalf("Findings = %v, want none", s.Findings)
	}
}

// Status must never print a credential value, even when every registered provider resolves one.
func TestStatusNeverPrintsAValue(t *testing.T) {
	setConfigDir(t)
	secretValue := "sk-super-secret-value-0123456789"
	t.Setenv("ANTHROPIC_API_KEY", secretValue)
	if err := SetFile("OPENROUTER_API_KEY", secretValue); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "") // keep deterministic: unset sources read as "none"

	reg, err := providers.Load()
	if err != nil {
		t.Fatalf("providers.Load: %v", err)
	}
	var buf bytes.Buffer
	Status(&buf, reg)
	out := buf.String()
	if strings.Contains(out, secretValue) {
		t.Fatalf("status output leaked a credential value:\n%s", out)
	}
	if !strings.Contains(out, "anthropic") || !strings.Contains(out, "env") {
		t.Errorf("status did not report anthropic as env-sourced:\n%s", out)
	}
	if !strings.Contains(out, "openrouter") || !strings.Contains(out, "file") {
		t.Errorf("status did not report openrouter as file-sourced:\n%s", out)
	}
}

// Doctor must flag secrets.env perms looser than 0600.
func TestDoctorFlagsLoosePerms(t *testing.T) {
	setConfigDir(t)
	if err := SetFile("A_KEY", "v"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(EnvFile(), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, _ := providers.Load()
	var buf bytes.Buffer
	if rc := Doctor(&buf, reg); rc != ExitFindings {
		t.Fatalf("Doctor rc = %d, want %d:\n%s", rc, ExitFindings, buf.String())
	}
}

// Doctor must flag an unknown key as a typo risk but still pass the gate (advisory only).
func TestDoctorPassesOnCleanStore(t *testing.T) {
	setConfigDir(t)
	if err := SetFile("ANTHROPIC_API_KEY", "v"); err != nil {
		t.Fatal(err)
	}
	reg, _ := providers.Load()
	var buf bytes.Buffer
	if rc := Doctor(&buf, reg); rc != ExitClean {
		t.Fatalf("Doctor rc = %d, want %d:\n%s", rc, ExitClean, buf.String())
	}
}

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
	rc := ScanGit(&buf, dir, 0)
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
	if rc := ScanGit(&buf, dir, 0); rc != ExitClean {
		t.Fatalf("ScanGit rc = %d, want %d:\n%s", rc, ExitClean, buf.String())
	}
}

// ScanGit outside any git repo must report clean (skipped), not an error.
func TestScanGitNotARepo(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if rc := ScanGit(&buf, dir, 0); rc != ExitClean {
		t.Fatalf("ScanGit rc = %d, want %d (skipped):\n%s", rc, ExitClean, buf.String())
	}
}

// Store indexes the name (never the value) and Delete drops it, so a keyring-only custom secret stays listable.
func TestStoreAndDeleteMaintainNamesIndex(t *testing.T) {
	setConfigDir(t)
	noKeyring(t)
	for _, n := range []string{"Z_KEY", "A_KEY", "Z_KEY"} {
		if _, err := Store(n, "secret-value", false); err != nil {
			t.Fatal(err)
		}
	}
	if got := Names(); !slices.Equal(got, []string{"A_KEY", "Z_KEY"}) {
		t.Fatalf("Names = %q, want [A_KEY Z_KEY]", got)
	}
	if b, _ := os.ReadFile(IndexFile()); strings.Contains(string(b), "secret-value") {
		t.Fatal("index must hold names only")
	}
	if err := Delete("Z_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := Names(); !slices.Equal(got, []string{"A_KEY"}) {
		t.Fatalf("Names after Delete = %q, want [A_KEY]", got)
	}
}
