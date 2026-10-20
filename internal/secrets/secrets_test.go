package secrets

import (
	"bytes"
	"encoding/json"
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
	if err := setFile("C_KEY", "v"); err != nil {
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
	if got := Redact("elevenchars"); got != "11 chars" {
		t.Errorf("Redact(11 chars) = %q, want length only", got)
	}
	// A pasted key shows just enough to tell it from a stray clipboard line, never the rest.
	long := "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"
	got := Redact(long)
	if want := "sk-an*** (" + strconv.Itoa(len(long)) + " chars)"; got != want {
		t.Errorf("Redact(long) = %q, want %q", got, want)
	}
	if strings.Contains(got, long[5:]) || strings.Contains(got, long[len(long)-4:]) {
		t.Errorf("Redact(long) leaked the value: %q", got)
	}
	if got := Redact("kJaxH1234567890ab"); got != "kJaxH*** (17 chars)" {
		t.Errorf("Redact(id) = %q", got)
	}
}

// setFile/Get must round-trip through the file plane, and the written file must be 0600.
func TestSetFileGetRoundTripAndPerms(t *testing.T) {
	setConfigDir(t)
	if err := setFile("GATEWAY_API_KEY", "sk-test-abc123"); err != nil {
		t.Fatalf("setFile: %v", err)
	}
	fi, err := os.Stat(EnvFile())
	if err != nil {
		t.Fatalf("stat %s: %v", EnvFile(), err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %04o, want 0600", perm)
	}
	val, source, ok := Get("GATEWAY_API_KEY")
	if !ok || val != "sk-test-abc123" || source != "file" {
		t.Fatalf("Get = (%q, %q, %v), want (sk-test-abc123, file, true)", val, source, ok)
	}
}

// A later setFile must replace the value, not duplicate the key or disturb an unrelated one.
func TestSetFileReplacesAndKeepsOthers(t *testing.T) {
	setConfigDir(t)
	if err := setFile("A_KEY", "first"); err != nil {
		t.Fatal(err)
	}
	if err := setFile("B_KEY", "untouched"); err != nil {
		t.Fatal(err)
	}
	if err := setFile("A_KEY", "second"); err != nil {
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

func TestScanFileClaudeMetadataFixtures(t *testing.T) {
	dir := t.TempDir()
	fixtures := map[string]string{
		".claude.json": `{"userID":"` + strings.Repeat("ab", 32) +
			`","machineID":"` + strings.Repeat("cd", 32) +
			`","projects":{"/home/u/.claude":{"lastSessionId":"12345678-1234-1234-1234-123456789abc"}}}`,
		"settings.json": `{"extraKnownMarketplaces":{"x":{"source":{"repo":"anthropics/claude-plugins-official"}}}}`,
	}
	var buf bytes.Buffer
	s := &Scanner{Out: &buf}
	for name, doc := range fixtures {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		s.ScanFile(path)
		if !strings.Contains(buf.String(), path+" — clean") {
			t.Errorf("%s did not scan clean", name)
		}
	}
	if len(s.Findings) != 0 {
		t.Fatalf("Findings = %v, want none", s.Findings)
	}
}

func TestScanFileMetadataExemptions(t *testing.T) {
	type scanCase struct {
		name   string
		file   string
		keys   []string
		exempt bool
	}
	cases := []scanCase{
		{"cache org", ".claude.json", []string{"clientDataCacheSlots", "slot", "org"}, true},
		{"marketplace", "settings.json", []string{"extraKnownMarketplaces", "x", "source", "repo"}, true},
		{"marketplace other JSON", "other.json", []string{"extraKnownMarketplaces", "x", "source", "repo"}, true},
		{"ordinary token", ".claude.json", []string{"token"}, false},
		{"userID other file", "settings.json", []string{"userID"}, false},
		{"cache other file", "settings.json", []string{"clientDataCacheSlots", "slot", "org"}, false},
		{"unrelated org", ".claude.json", []string{"other", "org"}, false},
		{"leaf prefix", ".claude.json", []string{"notuserID"}, false},
		{"leaf suffix", ".claude.json", []string{"userIDExtra"}, false},
		{"cache prefix", ".claude.json", []string{"notclientDataCacheSlots", "slot", "org"}, false},
		{"cache extra depth", ".claude.json", []string{"clientDataCacheSlots", "slot", "child", "org"}, false},
		{"marketplace prefix", "settings.json", []string{"notextraKnownMarketplaces", "x", "source", "repo"}, false},
		{"marketplace extra depth", "settings.json", []string{"extraKnownMarketplaces", "x", "child", "source", "repo"}, false},
	}
	for _, key := range []string{
		"userID", "machineID", "remoteControlMachineId", "lastSessionId", "sessionId",
		"accountUuid", "organizationUuid", "orgUuid", "experimentId", "originalHeadCommit",
	} {
		cases = append(cases, scanCase{key, ".claude.json", []string{"projects", "/home/u/.claude", key}, true})
	}
	values := []struct {
		name  string
		value string
		known bool
	}{
		{"hex", strings.Repeat("ab", 32), false},
		{"UUID", "12345678-1234-1234-1234-123456789abc", false},
		{"base64", strings.Repeat("Ab01", 10), false},
		{"sk", "sk-" + strings.Repeat("a", 40), true},
		{"github", "ghp_" + strings.Repeat("a", 40), true},
		{"AWS", "AKIA" + strings.Repeat("A", 16), true},
		{"JWT", "eyJheader.payload.signature", true},
		{"Slack", "xoxb-" + strings.Repeat("a", 40), true},
	}
	for _, tc := range cases {
		for _, value := range values {
			t.Run(tc.name+"/"+value.name, func(t *testing.T) {
				if !LooksLikeSecret(value.value) {
					t.Fatal("exported heuristic must still flag the value")
				}
				var doc any = value.value
				for _, k := range slices.Backward(tc.keys) {
					doc = map[string]any{k: doc}
				}
				raw, err := json.Marshal(doc)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), tc.file)
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				var buf bytes.Buffer
				s := &Scanner{Out: &buf}
				s.ScanFile(path)
				want := 0
				if !tc.exempt || value.known {
					want = 1
				}
				if len(s.Findings) != want {
					t.Fatalf("Findings = %v, want %d", s.Findings, want)
				}
				if want == 1 && !strings.Contains(s.Findings[0], strings.Join(tc.keys, ".")) {
					t.Error("finding must identify the key path")
				}
				if strings.Contains(buf.String(), value.value) || strings.Contains(strings.Join(s.Findings, "\n"), value.value) {
					t.Fatal("scan must never echo the value")
				}
			})
		}
	}
}

// Status must never print a credential value, even when every registered provider resolves one.
func TestStatusNeverPrintsAValue(t *testing.T) {
	dir := setConfigDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "env.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	gw := `{"providers":{"gateway":{"kind":"anthropic","base_url":"https://gateway.example.com/api","routes":{"openai":"/v1"},"auth":{"type":"bearer","env":"GATEWAY_API_KEY"},"models":{"opus":"anthropic/claude-opus-5","sonnet":"anthropic/claude-sonnet-5","haiku":"anthropic/claude-haiku-4-5"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "env.d", "providers.json"), []byte(gw), 0o600); err != nil {
		t.Fatal(err)
	}
	secretValue := "sk-super-secret-value-0123456789"
	t.Setenv("ANTHROPIC_API_KEY", secretValue)
	if err := setFile("GATEWAY_API_KEY", secretValue); err != nil {
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
	if !strings.Contains(out, "gateway") || !strings.Contains(out, "file") {
		t.Errorf("status did not report gateway as file-sourced:\n%s", out)
	}
}

// Doctor must flag secrets.env perms looser than 0600.
func TestDoctorFlagsLoosePerms(t *testing.T) {
	setConfigDir(t)
	if err := setFile("A_KEY", "v"); err != nil {
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
	if err := setFile("ANTHROPIC_API_KEY", "v"); err != nil {
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
	if b, _ := os.ReadFile(MetaFile()); strings.Contains(string(b), "secret-value") {
		t.Fatal("meta file must hold names only, never the value")
	}
	if err := Delete("Z_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := Names(); !slices.Equal(got, []string{"A_KEY"}) {
		t.Fatalf("Names after Delete = %q, want [A_KEY]", got)
	}
}

func TestPreviewShowsPrefixLengthAndDoubledPaste(t *testing.T) {
	key := "sk-ant-api03-" + strings.Repeat("a", 95)
	got := Preview(key)
	if !strings.HasPrefix(got, "sk-ant-api03-*") || !strings.HasSuffix(got, "(108 chars)") || strings.Contains(got, "aaa") {
		t.Errorf("Preview(anthropic) = %q", got)
	}
	if w := len([]rune("  pasted  " + Preview(key+key))); w > 80 {
		t.Errorf("doubled preview is %d columns", w)
	}
	if !strings.Contains(Preview(key+key), "pasted twice?") {
		t.Errorf("doubled paste not flagged: %q", Preview(key+key))
	}
	if got := Preview("abcdef"); got != "ab**** (6 chars)" {
		t.Errorf("short value reveals too much: %q", got)
	}
	if got := Preview("AIzaSyD" + strings.Repeat("x", 32)); !strings.HasPrefix(got, "AIza***") {
		t.Errorf("Preview(google) = %q", got)
	}
}

func TestPreviewPrefixStopsAtRandomSegment(t *testing.T) {
	cases := map[string]string{
		"tvly-dev-3vjVEqAbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdef": "tvly-dev-",
		"sk-ant-oat01-" + strings.Repeat("Z", 95):                   "sk-ant-oat01-",
		"tskey-api-kAbC123-" + strings.Repeat("q", 42):              "tskey-api-",
		"BBDC-" + strings.Repeat("A", 44):                           "BBDC",
	}
	for in, want := range cases {
		got := Preview(in)
		if !strings.HasPrefix(got, want+"*") {
			t.Errorf("Preview(%.20s…) = %q, want visible prefix %q then stars", in, got, want)
		}
	}
}
