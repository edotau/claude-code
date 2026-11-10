package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func currentVaultSchema(t *testing.T) int {
	t.Helper()
	for _, f := range schemas.All() {
		if f.Name == "vault" {
			return f.Version
		}
	}
	t.Fatal("vault schema is not registered")
	return 0
}

func TestScanVaultOverlaySecretsSorted(t *testing.T) {
	vaulttest.Use(t)
	const value = "sk-test-abcdefghijklmnopqrstuvwxyz0123456789"
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"d":{"k":"`+value+`"},"a":{"k":"`+value+`"},"c":{"k":"`+value+`"},"b":{"k":"`+value+`"},"e":{"k":"`+value+`"}}}`)
	var want []string
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		want = append(want, "vault:overlay/providers → providers."+n+".k holds an inline secret")
	}
	for range 20 {
		scanner := &Scanner{Out: &bytes.Buffer{}}
		scanner.ScanVault()
		if !slices.Equal(scanner.Findings, want) {
			t.Fatalf("findings = %v, want %v", scanner.Findings, want)
		}
	}
}

func TestScanVaultFindsInlineSecrets(t *testing.T) {
	vaulttest.Use(t)
	const value = "sk-test-abcdefghijklmnopqrstuvwxyz0123456789"
	vaulttest.Put(t, vault.Pins, "PIN_TOKEN", value)
	vaulttest.Put(t, vault.Env, "ENV_TOKEN", value)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"x":{"headers":[{"Authorization":"`+value+`"}]}}}`)
	vaulttest.Put(t, vault.Overlay, "providers.prev", `{"providers":{"x":{"auth":{"env":"`+value+`"}}}}`)
	vaulttest.Put(t, vault.Overlay, "agents", `{"agents":{"o":{"launch":{"env":["OPENCODE_DISABLE_CLAUDE_CODE_SKILLS=1","O_KEY=`+value+`"]}}}}`)
	vaulttest.Put(t, vault.Env, "STORED_REF", SecretRef)
	vaulttest.Put(t, vault.Pins, "ENV_REF", "${API_KEY}")
	vaulttest.Secret(t, "SEALED", value)
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	var out bytes.Buffer
	scanner := &Scanner{Out: &out}
	scanner.ScanVault()
	want := []string{
		"vault:overlay/providers → providers.x.headers.0.Authorization holds an inline secret",
		"vault:overlay/providers.prev → providers.x.auth.env holds an inline secret",
		"vault:overlay/agents → agents.o.launch.env.1 holds an inline secret",
		"vault:pins → PIN_TOKEN holds an inline secret",
		"vault:env → ENV_TOKEN holds an inline secret",
	}
	if !slices.Equal(scanner.Findings, want) {
		t.Fatalf("ScanVault findings = %v, want %v", scanner.Findings, want)
	}
	for _, finding := range want {
		if !strings.Contains(out.String(), finding) {
			t.Errorf("ScanVault output missing %q: %s", finding, out.String())
		}
	}
	if strings.Contains(out.String(), value) {
		t.Fatal("ScanVault printed a secret value")
	}
}

func TestScanVaultClean(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "populated"}[populated], func(t *testing.T) {
			vaulttest.Use(t)
			if populated {
				vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "openai")
				vaulttest.Put(t, vault.Env, "API_KEY", SecretRef)
				vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"x":{"auth":{"env":"API_KEY"}}}}`)
				vaulttest.Secret(t, "API_KEY", "sk-test-abcdefghijklmnopqrstuvwxyz")
			}
			t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
			var out bytes.Buffer
			scanner := &Scanner{Out: &out}
			scanner.ScanVault()
			if len(scanner.Findings) != 0 || !strings.Contains(out.String(), "vault — clean") {
				t.Fatalf("ScanVault = %v, output %s", scanner.Findings, out.String())
			}
			if !populated {
				if _, err := os.Stat(vault.Path()); !os.IsNotExist(err) {
					t.Fatalf("ScanVault created a vault: %v", err)
				}
			}
		})
	}
}

func TestScanVaultReadErrorIsFinding(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Meta, "schema", strconv.Itoa(currentVaultSchema(t)+1))
	var out bytes.Buffer
	scanner := &Scanner{Out: &out}
	scanner.ScanVault()
	if len(scanner.Findings) != 1 || !strings.Contains(scanner.Findings[0], "vault — could not read:") ||
		!strings.Contains(scanner.Findings[0], "schema "+strconv.Itoa(currentVaultSchema(t)+1)) || strings.Contains(out.String(), "vault — clean") {
		t.Fatalf("ScanVault must report unreadable vault: %v, %s", scanner.Findings, out.String())
	}
}

func TestScanConfigsDefaultIncludesVaultAndSummary(t *testing.T) {
	dir := vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	vaulttest.Put(t, vault.Env, "INLINE_KEY", "sk-test-abcdefghijklmnopqrstuvwxyz")
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	scanner := &Scanner{Out: &out}
	if code := scanner.ScanConfigs(nil); code != vault.ExitFindings {
		t.Fatalf("default scan = %d, want findings: %s", code, out.String())
	}
	if len(scanner.Findings) != 1 || scanner.Findings[0] != "vault:env → INLINE_KEY holds an inline secret" {
		t.Fatalf("default findings = %v", scanner.Findings)
	}
	want := "doctor --scan: 1 finding(s) — move inline values into the vault with `claude-code vault set`."
	if !strings.Contains(out.String(), path+" — clean") || !strings.Contains(out.String(), want) {
		t.Fatalf("default scan missing file scan or summary: %s", out.String())
	}
	out.Reset()
	scanner = &Scanner{Out: &out}
	if code := scanner.ScanConfigs([]string{path}); code != vault.ExitClean || len(scanner.Findings) != 0 {
		t.Fatalf("explicit file scan included vault: %d, %v, %s", code, scanner.Findings, out.String())
	}
}

func TestCrossCallerAgreeEnvWinsButGetPrefersVault(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Secret(t, "CROSS_NAME", "vault-value")
	t.Setenv("CROSS_NAME", "env-value")
	p := &Provider{
		Name: "x", Protocol: ProtocolAnthropic,
		Auth: Auth{Type: AuthKey, Header: HeaderXAPIKey, Env: "CROSS_NAME"},
	}
	cv, err := Credential(context.Background(), p)
	if err != nil || cv != "env-value" {
		t.Fatalf("Credential = (%q, %v), want (env-value, nil)", cv, err)
	}
	if st := Resolve(p); st.Source != "env" {
		t.Fatalf("Resolve(p).Source = %q, want env", st.Source)
	}
	if _, src, _, ok := vault.Effective("CROSS_NAME"); !ok || src != "env" {
		t.Fatalf("Effective source = %q (ok=%v), want env", src, ok)
	}
	if v, src, ok := vault.Get("CROSS_NAME"); !ok || v != "vault-value" || src != "vault" {
		t.Fatalf("Get = (%q, %q, %v), want (vault-value, vault, true)", v, src, ok)
	}
}

func TestScanConfigsMissingExplicitTargetIsFinding(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "typo.json")
	var out bytes.Buffer
	scanner := &Scanner{Out: &out}
	if code := scanner.ScanConfigs([]string{missing}); code != vault.ExitFindings {
		t.Fatalf("missing explicit target = %d, want findings: %s", code, out.String())
	}
	if len(scanner.Findings) != 1 || !strings.Contains(scanner.Findings[0], missing+" — could not read") {
		t.Fatalf("findings = %v", scanner.Findings)
	}
}

// LooksLikeSecret must flag a real-shaped token and ignore an ${ENV} reference.
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

// An unparseable .json is a finding, never clean; a non-.json file that is not JSON is scanned as KEY=value lines.
func TestScanFileParseFallback(t *testing.T) {
	const leak = "sk-abcdefghij0123456789"
	for _, tc := range []struct{ file, doc, want string }{
		{"settings.json", `{"env": {"K": "${K}"}}`, "clean"},
		{"broken.json", `{"env": {"K": "` + leak + `"`, "not valid JSON"},
		{"broken.JSON", `{not json`, "not valid JSON"},
		{"secrets.env", "K=${K}\n", "clean"},
		{"leak.env", "LEAKED=" + leak + "\n", "LEAKED holds an inline secret"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.file)
			writeTestFile(t, path, tc.doc, 0o600)
			var buf bytes.Buffer
			s := &Scanner{Out: &buf}
			s.ScanFile(path)
			clean := tc.want == "clean"
			if (len(s.Findings) == 0) != clean || !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("Findings = %v, output %q, want %q", s.Findings, buf.String(), tc.want)
			}
			if strings.Contains(buf.String(), leak) {
				t.Fatal("scan output must never echo the secret value")
			}
		})
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

// scanCase is one JSON key path in one file name, and whether the generic heuristic is exempt there.
type scanCase struct {
	name   string
	file   string
	keys   []string
	exempt bool
}

// scanValue is one string value; known marks a credential shape the exemption never covers.
type scanValue struct {
	name  string
	value string
	known bool
}

var scanExemptionCases = []scanCase{
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
	{"MCP header sessionId", ".claude.json", []string{"mcpServers", "api", "headers", "sessionId"}, false},
	{"project MCP env sessionId", ".claude.json", []string{"projects", "/home/x/y", "mcpServers", "s", "env", "sessionId"}, false},
	{"dotted project metadata", ".claude.json", []string{"projects", "/home/x/.config/project.name", "sessionId"}, true},
	{"dotted project MCP env", ".claude.json", []string{"projects", "/home/x/.config/project.name", "mcpServers", "s", "env", "sessionId"}, false},
	{"dotted project MCP header", ".claude.json", []string{"projects", "/home/x/.config/project.name", "mcpServers", "s", "headers", "sessionId"}, false},
	{"metadata wrapper", ".claude.json", []string{"wrapper", "sessionId"}, false},
	{"project metadata wrapper", ".claude.json", []string{"projects", "/home/x/y", "wrapper", "sessionId"}, false},
	{"wrapped project metadata", ".claude.json", []string{"wrapper", "projects", "/home/x/y", "sessionId"}, false},
	{"MCP marketplace", "settings.json", []string{"mcpServers", "s", "extraKnownMarketplaces", "x", "source", "repo"}, false},
	{"MCP header marketplace", "settings.json", []string{"mcpServers", "s", "headers", "extraKnownMarketplaces", "x", "source", "repo"}, false},
	{"dotted project prefix", ".claude.json", []string{"projects./home/x/y", "sessionId"}, false},
	{"dotted project leaf", ".claude.json", []string{"projects", "/home/x/y", "wrapper.sessionId"}, false},
	{"dotted cache prefix", ".claude.json", []string{"clientDataCacheSlots.slot", "org"}, false},
	{"dotted cache leaf", ".claude.json", []string{"clientDataCacheSlots", "slot.org"}, false},
	{"dotted marketplace prefix", "settings.json", []string{"extraKnownMarketplaces.x", "source", "repo"}, false},
	{"dotted marketplace container", "settings.json", []string{"extraKnownMarketplaces", "x.source", "repo"}, false},
	{"dotted marketplace leaf", "settings.json", []string{"extraKnownMarketplaces", "x", "source.repo"}, false},
	{"oauth account UUID", ".claude.json", []string{"oauthAccount", "accountUuid"}, true},
	{"oauth organization UUID", ".claude.json", []string{"oauthAccount", "organizationUuid"}, true},
	{"oauth other metadata", ".claude.json", []string{"oauthAccount", "sessionId"}, false},
	{"oauth extra depth", ".claude.json", []string{"oauthAccount", "wrapper", "accountUuid"}, false},
	{"dotted oauth hierarchy", ".claude.json", []string{"oauthAccount.accountUuid"}, false},
	{"worktree original commit", ".claude.json", []string{"projects", "/home/x/.config/project.name", "activeWorktreeSession", "originalHeadCommit"}, true},
	{"worktree session ID", ".claude.json", []string{"projects", "/home/x/y", "activeWorktreeSession", "sessionId"}, true},
	{"worktree extra depth", ".claude.json", []string{"projects", "/home/x/y", "wrapper", "activeWorktreeSession", "sessionId"}, false},
	{"worktree prefix", ".claude.json", []string{"projects", "/home/x/y", "notactiveWorktreeSession", "sessionId"}, false},
	{"dotted worktree hierarchy", ".claude.json", []string{"projects", "/home/x/y", "activeWorktreeSession.sessionId"}, false},
	{"cached experiment ID", ".claude.json", []string{"cachedExperimentData", "experiment.name", "experimentId"}, true},
	{"cached experiment extra depth", ".claude.json", []string{"cachedExperimentData", "experiment", "wrapper", "experimentId"}, false},
	{"cached experiment prefix", ".claude.json", []string{"notcachedExperimentData", "experiment", "experimentId"}, false},
	{"promo account UUID", ".claude.json", []string{"promoStartupStatusCache", "accountUuid"}, true},
	{"artifact account UUID", ".claude.json", []string{"cachedArtifactRoster", "accountUuid"}, true},
	{"artifact organization UUID", ".claude.json", []string{"cachedArtifactRoster", "organizationUuid"}, true},
	{"github account UUID", ".claude.json", []string{"githubWebConnectionStatusCache", "accountUuid"}, true},
	{"github org UUID", ".claude.json", []string{"githubWebConnectionStatusCache", "orgUuid"}, true},
	{"usage account UUID", ".claude.json", []string{"cachedUsageUtilization", "accountUuid"}, true},
	{"chrome paired device ID", ".claude.json", []string{"chromeExtension", "pairedDeviceId"}, true},
	{"chrome other field", ".claude.json", []string{"chromeExtension", "token"}, false},
	{"usage extra depth", ".claude.json", []string{"cachedUsageUtilization", "wrapper", "accountUuid"}, false},
	{"usage prefix", ".claude.json", []string{"notcachedUsageUtilization", "accountUuid"}, false},
	{"usage other metadata", ".claude.json", []string{"cachedUsageUtilization", "sessionId"}, false},
	{"usage other file", "settings.json", []string{"cachedUsageUtilization", "accountUuid"}, false},
}

// metadataLeafKeys are the claude metadata keys exempt both at top level and under a project.
var metadataLeafKeys = []string{
	"userID", "machineID", "remoteControlMachineId", "lastSessionId", "sessionId",
	"accountUuid", "organizationUuid", "orgUuid", "experimentId", "originalHeadCommit",
}

var scanValues = []scanValue{
	{"hex", strings.Repeat("ab", 32), false},
	{"UUID", "12345678-1234-1234-1234-123456789abc", false},
	{"base64", strings.Repeat("Ab01", 10), false},
	{"sk", "sk-" + strings.Repeat("a", 40), true},
	{"github", "ghp_" + strings.Repeat("a", 40), true},
	{"AWS", "AKIA" + strings.Repeat("A", 16), true},
	{"JWT", "eyJheader.payload.signature", true},
	{"Slack", "xoxb-" + strings.Repeat("a", 40), true},
}

func TestScanFileMetadataExemptions(t *testing.T) {
	cases := slices.Clone(scanExemptionCases)
	for _, key := range metadataLeafKeys {
		cases = append(cases, scanCase{key, ".claude.json", []string{"projects", "/home/u/.claude", key}, true})
		cases = append(cases, scanCase{"top-level " + key, ".claude.json", []string{key}, true})
	}
	for _, tc := range cases {
		for _, value := range scanValues {
			t.Run(tc.name+"/"+value.name, func(t *testing.T) { checkScanExemption(t, tc, value) })
		}
	}
}

// checkScanExemption scans a file holding value at tc.keys and asserts the finding count and that nothing echoes the value.
func checkScanExemption(t *testing.T, tc scanCase, value scanValue) {
	t.Helper()
	s, out := scanNested(t, tc, value.value)
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
	if strings.Contains(out.String(), value.value) || strings.Contains(strings.Join(s.Findings, "\n"), value.value) {
		t.Fatal("scan must never echo the value")
	}
}

// scanNested writes value nested under tc.keys into tc.file and scans it, returning the scanner and its output.
func scanNested(t *testing.T, tc scanCase, value string) (*Scanner, *bytes.Buffer) {
	t.Helper()
	if !vault.LooksLikeSecret(value) {
		t.Fatal("exported heuristic must still flag the value")
	}
	var doc any = value
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
	return s, &buf
}
