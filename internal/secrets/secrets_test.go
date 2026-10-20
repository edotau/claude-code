package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func noKeyring(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func wrongVaultKey(t *testing.T) {
	t.Helper()
	vaulttest.Keyring(t)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)))
}

func TestStoreUsesKeyringMasterKey(t *testing.T) {
	dir, _ := vaulttest.UseKeyring(t)
	if err := Store("A_KEY", "v"); err != nil {
		t.Fatal(err)
	}
	if value, source, err := Lookup("A_KEY"); err != nil || value != "v" || source != "vault" {
		t.Fatalf("Lookup = (%q, %q, %v), want (v, vault, nil)", value, source, err)
	}
	if _, source, err := vault.MasterKey(); err != nil || source != "keyring" {
		t.Fatalf("MasterKey source = %q, %v, want keyring, nil", source, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "env.d")); !os.IsNotExist(err) {
		t.Fatalf("Store must not create env.d: %v", err)
	}
}

func TestStoreWithoutMasterKeyFails(t *testing.T) {
	_, kr := vaulttest.UseKeyring(t)
	kr.Down = true
	if err := Store("B_KEY", "v"); !errors.Is(err, vault.ErrLocked) {
		t.Fatalf("Store = %v, want vault.ErrLocked", err)
	}
	if got := Names(); len(got) != 0 {
		t.Fatalf("failed Store persisted names: %v", got)
	}
}

func TestStoreRejectsMultiline(t *testing.T) {
	for _, value := range []string{"a\nb", "a\rb", "a\r\nb", "\na", "a\n"} {
		t.Run(strconv.Quote(value), func(t *testing.T) {
			vaulttest.Use(t)
			vaulttest.Secret(t, "ONE_LINE", "unchanged")
			before := readRecord(t, "ONE_LINE")
			if err := Store("ONE_LINE", value); err == nil || err.Error() != "ONE_LINE: a secret must be one line" {
				t.Fatalf("Store multiline = %v", err)
			}
			after := readRecord(t, "ONE_LINE")
			if !bytes.Equal(before.Sealed, after.Sealed) || before.Kid != after.Kid || !before.SetAt.Equal(after.SetAt) {
				t.Fatal("rejected Store changed the existing record")
			}
		})
	}
}

func TestDeleteRemovesVaultRecordWithoutKey(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "C_KEY", "v")
	vaulttest.Secret(t, "KEPT", "untouched")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	t.Setenv("C_KEY", "env-value")
	if err := Delete("C_KEY"); err != nil {
		t.Fatal(err)
	}
	if got := Names(); !slices.Equal(got, []string{"KEPT"}) {
		t.Fatalf("Names after Delete = %v, want [KEPT]", got)
	}
	if value, source, err := Lookup("C_KEY"); err != nil || value != "env-value" || source != "env" {
		t.Fatalf("Lookup after Delete = (%q, %q, %v), want (env-value, env, nil)", value, source, err)
	}
}

// Delete on a name stored nowhere must be a no-op, not an error.
func TestDeleteMissingIsNoop(t *testing.T) {
	vaulttest.Use(t)
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

func TestStoreGetRoundTripAndPerms(t *testing.T) {
	vaulttest.Use(t)
	if err := Store("GATEWAY_API_KEY", "sk-test-abc123"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	fi, err := os.Stat(vault.Path())
	if err != nil {
		t.Fatalf("stat %s: %v", vault.Path(), err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %04o, want 0600", perm)
	}
	val, source, ok := Get("GATEWAY_API_KEY")
	if !ok || val != "sk-test-abc123" || source != "vault" {
		t.Fatalf("Get = (%q, %q, %v), want (sk-test-abc123, vault, true)", val, source, ok)
	}
}

func TestStoreReplacesAndKeepsOthers(t *testing.T) {
	vaulttest.Use(t)
	if err := Store("A_KEY", "first"); err != nil {
		t.Fatal(err)
	}
	if err := Store("B_KEY", "untouched"); err != nil {
		t.Fatal(err)
	}
	if err := Store("A_KEY", "second"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"A_KEY": "second", "B_KEY": "untouched"} {
		if got, source, err := Lookup(name); err != nil || got != want || source != "vault" {
			t.Errorf("Lookup(%s) = (%q, %q, %v), want (%q, vault, nil)", name, got, source, err, want)
		}
	}
	if got := Names(); !slices.Equal(got, []string{"A_KEY", "B_KEY"}) {
		t.Fatalf("Names = %v, want [A_KEY B_KEY]", got)
	}
}

func TestGetFallsThroughToEnvOnlyWhenVaultEmpty(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("ONLY_IN_ENV", " from-process-env \n")
	val, source, ok := Get("ONLY_IN_ENV")
	if !ok || val != "from-process-env" || source != "env" {
		t.Fatalf("Get = (%q, %q, %v), want (from-process-env, env, true)", val, source, ok)
	}
}

// Get on a name set nowhere must miss cleanly, never an error or a panic.
func TestGetMiss(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("NEVER_SET", " \n")
	if v, src, ok := Get("NEVER_SET"); ok || v != "" || src != "" {
		t.Fatal("Get on an unset name must report ok=false")
	}
}

func TestLookupVaultBeforeEnvAndPreservesError(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "LOOKUP", "vault-value")
	vaulttest.Secret(t, "LOOKUP_NO_ENV", "vault-value")
	t.Setenv("LOOKUP", "env-value")
	if value, source, err := Lookup("LOOKUP"); err != nil || value != "vault-value" || source != "vault" {
		t.Fatalf("Lookup = (%q, %q, %v), want (vault-value, vault, nil)", value, source, err)
	}

	// A vault read error with a non-empty env var falls back to it rather than failing outright.
	wrongVaultKey(t)
	if value, source, err := Lookup("LOOKUP"); err != nil || value != "env-value" || source != "env" {
		t.Fatalf("Lookup with wrong key, env set = (%q, %q, %v), want (env-value, env, nil)", value, source, err)
	}
	if value, source, ok := Get("LOOKUP"); !ok || value != "env-value" || source != "env" {
		t.Fatalf("Get with wrong key, env set = (%q, %q, %v), want (env-value, env, true)", value, source, ok)
	}

	// Same locked vault, no env var: the read error surfaces.
	if value, source, err := Lookup("LOOKUP_NO_ENV"); !errors.Is(err, vault.ErrWrongKey) || value != "" || source != "" {
		t.Fatalf("Lookup with wrong key, no env = (%q, %q, %v), want empty, empty, ErrWrongKey", value, source, err)
	}
	if value, source, ok := Get("LOOKUP_NO_ENV"); ok || value != "" || source != "" {
		t.Fatalf("Get with wrong key, no env = (%q, %q, %v), want empty, empty, false", value, source, ok)
	}

	t.Setenv("ENV_FALLBACK", " env-value ")
	if value, source, err := Lookup("ENV_FALLBACK"); err != nil || value != "env-value" || source != "env" {
		t.Fatalf("Lookup absent record = (%q, %q, %v), want (env-value, env, nil)", value, source, err)
	}
}

func TestLookupMissingAndEmptyValue(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("LOOKUP_EMPTY", "")
	if value, source, err := Lookup("LOOKUP_EMPTY"); err != nil || value != "" || source != "" {
		t.Fatalf("Lookup missing = (%q, %q, %v), want empty, empty, nil", value, source, err)
	}
	if err := Store("LOOKUP_EMPTY", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOKUP_EMPTY", "env-value")
	if value, source, err := Lookup("LOOKUP_EMPTY"); err != nil || value != "" || source != "vault" {
		t.Fatalf("Lookup empty record = (%q, %q, %v), want empty, vault, nil", value, source, err)
	}
	if value, source, ok := Get("LOOKUP_EMPTY"); ok || value != "" || source != "vault" {
		t.Fatalf("Get empty record = (%q, %q, %v), want empty, vault, false", value, source, ok)
	}
}

func TestNamesUnreadableVault(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Meta, "schema", "3")
	if got := Names(); got != nil {
		t.Fatalf("Names unreadable vault = %v, want nil", got)
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
		{"usage extra depth", ".claude.json", []string{"cachedUsageUtilization", "wrapper", "accountUuid"}, false},
		{"usage prefix", ".claude.json", []string{"notcachedUsageUtilization", "accountUuid"}, false},
		{"usage other metadata", ".claude.json", []string{"cachedUsageUtilization", "sessionId"}, false},
		{"usage other file", "settings.json", []string{"cachedUsageUtilization", "accountUuid"}, false},
	}
	for _, key := range []string{
		"userID", "machineID", "remoteControlMachineId", "lastSessionId", "sessionId",
		"accountUuid", "organizationUuid", "orgUuid", "experimentId", "originalHeadCommit",
	} {
		cases = append(cases, scanCase{key, ".claude.json", []string{"projects", "/home/u/.claude", key}, true})
		cases = append(cases, scanCase{"top-level " + key, ".claude.json", []string{key}, true})
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
	vaulttest.Use(t)
	gw := `{"providers":{"gateway":{"kind":"anthropic","base_url":"https://gateway.example.com/api","routes":{"openai":"/v1"},"auth":{"type":"bearer","env":"GATEWAY_API_KEY"},"models":{"opus":"anthropic/claude-opus-5","sonnet":"anthropic/claude-sonnet-5","haiku":"anthropic/claude-haiku-4-5"}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", gw)
	secretValue := "sk-super-secret-value-0123456789"
	t.Setenv("ANTHROPIC_API_KEY", secretValue)
	vaulttest.Secret(t, "GATEWAY_API_KEY", secretValue)
	t.Setenv("GATEWAY_API_KEY", "")
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	t.Setenv("OPENAI_API_KEY", "") // keep deterministic: unset sources read as "none"

	reg, err := providers.Load()
	if err != nil {
		t.Fatalf("providers.Load: %v", err)
	}
	if got := Resolve(reg.Providers["gateway"]); got.Source != "vault" {
		t.Fatalf("Resolve(gateway) = %+v, want vault source without decryption", got)
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
	if !strings.Contains(out, "gateway") || !strings.Contains(out, "vault") {
		t.Errorf("status did not report gateway as vault-sourced:\n%s", out)
	}
}

// Doctor must flag vault.db perms looser than 0600.
func TestDoctorFlagsLoosePerms(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "A_KEY", "v")
	if err := os.Chmod(vault.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if rc := Doctor(&buf, reg); rc != ExitFindings {
		t.Fatalf("Doctor rc = %d, want %d:\n%s", rc, ExitFindings, buf.String())
	}
	if !strings.Contains(buf.String(), vault.Path()+" is 0644, want 0600") || !strings.Contains(buf.String(), "chmod 600 "+vault.Path()) {
		t.Fatalf("Doctor missing vault permission remediation: %s", buf.String())
	}
}

// Doctor must flag an unknown key as a typo risk but still pass the gate (advisory only).
func TestDoctorPassesOnCleanStore(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Secret(t, "UNKNOWN_KEY", "v")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if rc := Doctor(&buf, reg); rc != ExitClean {
		t.Fatalf("Doctor rc = %d, want %d:\n%s", rc, ExitClean, buf.String())
	}
	if !strings.Contains(buf.String(), "UNKNOWN_KEY matches no provider's auth.env") ||
		!strings.Contains(buf.String(), "1 secret(s); master key from env") {
		t.Fatalf("Doctor missing advisory or key source: %s", buf.String())
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

// Names remain listable without decryption, including custom secrets unknown to the provider registry.
func TestStoreAndDeleteMaintainNamesIndex(t *testing.T) {
	vaulttest.Use(t)
	for _, n := range []string{"Z_KEY", "A_KEY", "Z_KEY"} {
		if err := Store(n, "secret-value"); err != nil {
			t.Fatal(err)
		}
	}
	if got := Names(); !slices.Equal(got, []string{"A_KEY", "Z_KEY"}) {
		t.Fatalf("Names = %q, want [A_KEY Z_KEY]", got)
	}
	t.Setenv("HARNESS_VAULT_KEY", "invalid-key")
	t.Setenv("ONLY_IN_ENV", "not-stored")
	if got := Names(); !slices.Equal(got, []string{"A_KEY", "Z_KEY"}) {
		t.Fatalf("Names without key = %q, want [A_KEY Z_KEY]", got)
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

// TestLookupVaultErrorFallsBackToEnvOnlyWhenSet: a vault read error (locked key, not "missing")
// returns a non-empty env var with source "env" and no error; with the env var unset, the same
// locked vault surfaces the read error instead of silently reporting a miss.
func TestLookupVaultErrorFallsBackToEnvOnlyWhenSet(t *testing.T) {
	_, kr := vaulttest.UseKeyring(t)
	vaulttest.Secret(t, "LOCKED_FALLBACK", "vault-value")
	kr.Down = true

	t.Setenv("LOCKED_FALLBACK", "env-value")
	if value, source, err := Lookup("LOCKED_FALLBACK"); err != nil || value != "env-value" || source != "env" {
		t.Fatalf("Lookup(env set) = (%q, %q, %v), want (env-value, env, nil)", value, source, err)
	}

	t.Setenv("LOCKED_FALLBACK", "")
	if value, source, err := Lookup("LOCKED_FALLBACK"); err == nil || value != "" || source != "" {
		t.Fatalf("Lookup(env unset) = (%q, %q, %v), want empty, empty, a read error", value, source, err)
	}
}
