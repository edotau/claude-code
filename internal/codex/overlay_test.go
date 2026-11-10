package codex_test

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/agentconfig"
	agentrender "github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/agentconfig/render/rendertest"
	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestConfigHistoryIsEncryptedBoundedAndSurvivesRotation(t *testing.T) {
	dir, _ := vaulttest.UseKeyring(t)
	path := filepath.Join(dir, "native", "config.toml")
	other := filepath.Join(dir, "other", "config.toml")
	fixture := []byte("private-config-fixture-68b19d\nforeign = 'preserve exactly'\n")
	seedCodexConfigHistory(t, path, other, fixture)
	assertCodexConfigHistoryEncrypted(t, dir, fixture)
	oldKid, newKid, err := vault.RotateKey()
	if err != nil || oldKid == newKid {
		t.Fatalf("rotation did not change the key: %v", err)
	}
	assertCodexRotatedContents(t, path, fixture)
	assertCodexRotatedMetadata(t, newKid)
	consumeCodexHistoryVersions(t, path)
	if _, found, err := codex.LatestCodexBackup(path); err != nil || found {
		t.Fatalf("more than five snapshots retained: found=%v, err=%v", found, err)
	}
	assertCodexHistoryOtherAndBaseline(t, path, other, fixture)
}

func seedCodexConfigHistory(t *testing.T, path, other string, fixture []byte) {
	t.Helper()
	if err := codex.SaveCodexBaseline(path, fixture); err != nil {
		t.Fatal(err)
	}
	for i := range 7 {
		if err := codex.CodexBackup(path, fmt.Appendf(nil, "version %d", i), true); err != nil {
			t.Fatal(err)
		}
	}
	if err := codex.CodexBackup(other, fixture, false); err != nil {
		t.Fatal(err)
	}
	vaulttest.Secret(t, "API_KEY", "credential-fixture")
}

func assertCodexConfigHistoryEncrypted(t *testing.T, dir string, fixture []byte) {
	t.Helper()
	pairs, err := vault.ExportPairs(nil)
	if err != nil || !reflect.DeepEqual(pairs, []vault.Pair{{Name: "API_KEY", Value: "credential-fixture"}}) {
		t.Fatalf("credential export includes unexpected entries: %v, %v", pairs, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state", "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, clear := range [][]byte{fixture, []byte(base64.StdEncoding.EncodeToString(fixture)), []byte("private-config-fixture-68b19d")} {
		if bytes.Contains(raw, clear) {
			t.Fatal("vault contains plaintext config contents")
		}
	}
}

func assertCodexRotatedContents(t *testing.T, path string, fixture []byte) {
	t.Helper()
	baseline, err := codex.LoadCodexBaseline(path)
	if err != nil || !bytes.Equal(baseline, fixture) {
		t.Fatalf("baseline did not survive rotation: %v", err)
	}
	pairs, err := vault.ExportPairs(nil)
	if err != nil || !reflect.DeepEqual(pairs, []vault.Pair{{Name: "API_KEY", Value: "credential-fixture"}}) {
		t.Fatalf("credential did not survive rotation: %v", err)
	}
}

func assertCodexRotatedMetadata(t *testing.T, newKid string) {
	t.Helper()
	if err := vault.View(func(tx *vault.Tx) error {
		for _, name := range tx.Names(vault.SettingsBackups) {
			r, found, err := tx.RecordIn(vault.SettingsBackups, name)
			if err != nil || !found || r.Kid != newKid || r.Prefix != "" {
				t.Errorf("backup metadata after rotation: found=%v, kid=%q, prefix=%q, err=%v", found, r.Kid, r.Prefix, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func consumeCodexHistoryVersions(t *testing.T, path string) {
	t.Helper()
	for i := 6; i >= 2; i-- {
		b, found, err := codex.LatestCodexBackup(path)
		if err != nil || !found || string(b.Data) != fmt.Sprintf("version %d", i) || !b.Existed || b.Path != path {
			t.Fatalf("latest backup %d: found=%v, data=%q, err=%v", i, found, b.Data, err)
		}
		if err := codex.ConsumeCodexBackup(b.Name); err != nil {
			t.Fatal(err)
		}
	}
}

func assertCodexHistoryOtherAndBaseline(t *testing.T, path, other string, fixture []byte) {
	t.Helper()
	b, found, err := codex.LatestCodexBackup(other)
	if err != nil || !found || !bytes.Equal(b.Data, fixture) || b.Existed || b.Path != other {
		t.Fatalf("other path snapshot changed: found=%v, err=%v", found, err)
	}
	baseline, err := codex.LoadCodexBaseline(path)
	if err != nil || !bytes.Equal(baseline, fixture) {
		t.Fatalf("snapshot consumption changed baseline: %v", err)
	}
}

func TestCredentialAndConfigCiphertextCannotBeExchanged(t *testing.T) {
	vaulttest.UseKeyring(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := codex.CodexBackup(path, []byte("original native config"), true); err != nil {
		t.Fatal(err)
	}
	b, found, err := codex.LatestCodexBackup(path)
	if err != nil || !found {
		t.Fatalf("backup missing: %v", err)
	}
	vaulttest.Secret(t, b.Name, "credential using identical record name")
	if err := vault.Update(func(tx *vault.Tx) error {
		credential, _, err := tx.Record(b.Name)
		if err != nil {
			return err
		}
		backup, _, err := tx.RecordIn(vault.SettingsBackups, b.Name)
		if err != nil {
			return err
		}
		if err := tx.PutRecord(b.Name, backup); err != nil {
			return err
		}
		return tx.PutRecordIn(vault.SettingsBackups, b.Name, credential)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := vault.ReadSecret(b.Name); err == nil {
		t.Fatal("credential collection decrypted settings ciphertext")
	}
	if _, _, err := vault.ReadSettingsBackup(b.Name); err == nil {
		t.Fatal("settings collection decrypted credential ciphertext")
	}
}

func TestPreferencesRejectInvalidEditsWithoutOverwritingSavedValues(t *testing.T) {
	vaulttest.Use(t)
	want := map[string]string{"model_reasoning_effort": "high", "model_verbosity": "low"}
	if err := codex.SaveCodexOverlay(want); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []map[string]string{
		{"model_reasoning_effort": "unbounded"},
		{"model_verbosity": "verbose"},
		{"max_output_tokens": "256"},
	} {
		if err := codex.SaveCodexOverlay(invalid); err == nil {
			t.Fatalf("accepted invalid preferences: %v", invalid)
		}
		got, err := codex.LoadCodexOverlay()
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("invalid edit overwrote saved preferences: %v, %v", got, err)
		}
	}
	if err := codex.UpdateCodexOverlay(func(current map[string]string) map[string]string {
		current["model_verbosity"] = "medium"
		return current
	}); err != nil {
		t.Fatal(err)
	}
	want["model_verbosity"] = "medium"
	got, err := codex.LoadCodexOverlay()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("staged preference merge lost an untouched preference: %v, %v", got, err)
	}
}

// codex.MergeCodexTrust must not mistake a "["-led line inside an open array for a table header: an
// existing trusted_hash AFTER the array, still inside the same table, is replaced in place —
// not left stale with a duplicate appended at EOF because the array wrongly reset the active table.
func TestMergeCodexTrustBracketInArrayNotHeader(t *testing.T) {
	raw := "[hooks.state.\"K\"]\nextra = [\n  [1, 2],\n]\ntrusted_hash = \"old\"\n"
	entries := map[string]string{"K": "abc123"}
	got := codex.MergeCodexTrust(raw, entries)
	want := "[hooks.state.\"K\"]\nextra = [\n  [1, 2],\n]\ntrusted_hash = \"abc123\"\n"
	if got != want {
		t.Fatalf("codex.MergeCodexTrust:\n%q\nwant:\n%q", got, want)
	}
	if strings.Count(got, "trusted_hash") != 1 {
		t.Errorf("want exactly one trusted_hash line, got:\n%q", got)
	}
}

// A real table header following a "["-led array element must still pick up its own hash, proving
// the depth tracking resets once the array closes rather than staying stuck open.
func TestMergeCodexTrustHeaderAfterArrayStillMatches(t *testing.T) {
	raw := "extra = [\n  [1, 2],\n]\n[hooks.state.\"K2\"]\nother = 1\n"
	entries := map[string]string{"K2": "def456"}
	got := codex.MergeCodexTrust(raw, entries)
	want := "extra = [\n  [1, 2],\n]\n[hooks.state.\"K2\"]\nother = 1\ntrusted_hash = \"def456\"\n"
	if got != want {
		t.Fatalf("codex.MergeCodexTrust:\n%q\nwant:\n%q", got, want)
	}
}

func TestMergeCodexTrustPreservesUnownedText(t *testing.T) {
	raw := `note = """
[hooks.state."active"]
trusted_hash = "inside string"
"""
[hooks.state.'active'] # keep table comment
  'trusted_hash' = 'old' # keep hash comment
other = 1
[hooks.state."missing"]
other = 2
[foreign]
trusted_hash = "unowned"`
	want := `note = """
[hooks.state."active"]
trusted_hash = "inside string"
"""
[hooks.state.'active'] # keep table comment
  'trusted_hash' = "new" # keep hash comment
other = 1
[hooks.state."missing"]
other = 2
trusted_hash = "added"
[foreign]
trusted_hash = "unowned"

[hooks.state."a"]
trusted_hash = "first"

[hooks.state."z"]
trusted_hash = "last"
`
	entries := map[string]string{"active": "new", "missing": "added", "z": "last", "a": "first"}
	got := codex.MergeCodexTrust(raw, entries)
	if got != want {
		t.Fatalf("codex.MergeCodexTrust:\n%s\nwant:\n%s", got, want)
	}
	if again := codex.MergeCodexTrust(got, entries); again != got {
		t.Fatalf("merge is not idempotent:\n%s", again)
	}
	if len(entries) != 4 {
		t.Fatal("merge mutated input entries")
	}
}

// TestInsertTopLevelTOMLSkipsMultilineStrings: a "["-led line inside a TOML multiline string
// (triple-quoted) must not be mistaken for the first top-level table header.
func TestInsertTopLevelTOMLSkipsMultilineStrings(t *testing.T) {
	cases := []struct {
		name  string
		quote string
	}{
		{"doubleQuoted", `"""`},
		{"singleQuoted", `'''`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := "note = " + c.quote + "\nsome text\n[looks like a table]\nmore text\n" + c.quote + "\n\n[real.table]\nkey = 1\n"
			section := "[inserted]\nx = 1\n"
			got := agentconfig.InsertTopLevelTOML(in, section)

			wantString := "note = " + c.quote + "\nsome text\n[looks like a table]\nmore text\n" + c.quote + "\n"
			idxString := strings.Index(got, wantString)
			idxInserted := strings.Index(got, "[inserted]")
			idxRealTable := strings.Index(got, "[real.table]")

			if idxString == -1 {
				t.Fatalf("multiline string content not byte-for-byte unchanged, got:\n%s", got)
			}
			if idxInserted == -1 || idxRealTable == -1 {
				t.Fatalf("missing expected sections, got:\n%s", got)
			}
			if idxString >= idxInserted || idxInserted >= idxRealTable {
				t.Errorf("want order: multiline string, then [inserted], then [real.table]; got:\n%s", got)
			}
		})
	}
}

// TestInsertTopLevelTOMLSkipsNestedArrays: a "["-led line inside an open multiline array (nested
// array element, not a multiline string) must not be mistaken for the first top-level table header.
func TestInsertTopLevelTOMLSkipsNestedArrays(t *testing.T) {
	in := "matrix = [\n  [1, 2],\n  [3, 4],\n]\n\n[real]\nk = 1\n"
	section := "[inserted]\nx = 1\n"
	got := agentconfig.InsertTopLevelTOML(in, section)

	wantArray := "matrix = [\n  [1, 2],\n  [3, 4],\n]\n"
	idxArray := strings.Index(got, wantArray)
	idxInserted := strings.Index(got, "[inserted]")
	idxReal := strings.Index(got, "[real]")

	if idxArray == -1 {
		t.Fatalf("array bytes not byte-for-byte unchanged, got:\n%s", got)
	}
	if idxInserted == -1 || idxReal == -1 {
		t.Fatalf("missing expected sections, got:\n%s", got)
	}
	if idxArray >= idxInserted || idxInserted >= idxReal {
		t.Errorf("want order: array, then [inserted], then [real]; got:\n%s", got)
	}
}

// foreignMarker is the literal foreign-hook command a third-party tool (e.g. an agent dashboard) installs into every
// lifecycle event; real bytes captured in testdata/codex-hooks-with-foreign.json (= ~/.codex/hooks.json.backup).
const foreignMarker = "foreign-agent-hook-form=1"

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRenderCodexHooksFilePreservesForeignOnDisk is the end-to-end RED/GREEN: seed a scratch ~/.codex/hooks.json
// with the real pre-render bytes, run the full Codex render, and show the foreign hooks survive on disk.
func TestRenderCodexHooksFilePreservesForeignOnDisk(t *testing.T) {
	home := rendertest.Scratch(t)
	hooksPath := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, readTestdata(t, "codex-hooks-with-foreign.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := client.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), foreignMarker); n != 9 {
		t.Fatalf("expected all 9 foreign groups to survive the render, found %d:\n%s", n, got)
	}
	if !strings.Contains(string(got), "claude-code hook PreToolUse --safety-scan") {
		t.Errorf("rendered codex hooks missing the current harness hook:\n%s", got)
	}
}

// TestRenderCodexFailsHooksMergeLeavesConfigTomlUntouched is the RED fixture for partial-apply: a
// malformed existing ~/.codex/hooks.json must fail the whole render before config.toml (new trust hashes)
// is ever written, not after.
func TestRenderCodexFailsHooksMergeLeavesConfigTomlUntouched(t *testing.T) {
	home := rendertest.Scratch(t)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const originalToml = "# pre-existing config, untouched\n"
	tomlPath := filepath.Join(codexDir, "config.toml")
	if err := os.WriteFile(tomlPath, []byte(originalToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "hooks.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err := client.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}})
	if err == nil {
		t.Fatalf("expected client.RenderConfigs to fail on a malformed hooks.json, got no error:\n%s", out.String())
	}
	got, readErr := os.ReadFile(tomlPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != originalToml {
		t.Fatalf("config.toml was written despite the hooks merge failing:\nwant:\n%s\ngot:\n%s", originalToml, got)
	}
}

// seedCodexFile writes body to home/.codex/name and returns its path.
func seedCodexFile(t *testing.T, home, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(home, ".codex", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A hooks.json changed between prepare and apply fails the render before config.toml gets new trust hashes.
func TestRenderCodexHooksWriteConflictLeavesConfigTomlUntouched(t *testing.T) {
	home := rendertest.Scratch(t)
	originalToml := []byte("# existing config must survive a failed hooks write\n")
	tomlPath := seedCodexFile(t, home, "config.toml", originalToml)
	hooksPath := seedCodexFile(t, home, "hooks.json", []byte(`{"hooks":{}}`))
	var out bytes.Buffer
	plan, drift, err := client.PrepareConfigs(agentrender.RenderOptions{
		Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"},
	})
	if err != nil || !drift {
		t.Fatalf("prepare: drift=%v, err=%v\n%s", drift, err, out.String())
	}
	newerHooks := []byte(`{"hooks":{},"foreign":"concurrent edit"}`)
	seedCodexFile(t, home, "hooks.json", newerHooks)
	if err := plan.Apply(); err == nil || !strings.HasPrefix(err.Error(), "codex-hooks:") ||
		!strings.Contains(err.Error(), "changed since it was read") {
		t.Fatalf("apply: expected hooks concurrency error, got %v\n%s", err, out.String())
	}
	for path, want := range map[string][]byte{tomlPath: originalToml, hooksPath: newerHooks} {
		if got, _ := os.ReadFile(path); !bytes.Equal(got, want) {
			t.Errorf("%s changed after the failed hooks write:\nwant:\n%s\ngot:\n%s", filepath.Base(path), want, got)
		}
	}
}

// Approval/sandbox top-level keys land before the first [table] header, never inside one, and a
// second run is byte-identical (idempotent), even with pre-existing foreign tables in the file.
func TestCodexApprovalKeysPrecedeExistingTables(t *testing.T) {
	home := rendertest.Scratch(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := "[projects.\"/x\"]\ntrust_level = \"trusted\"\n\n[hooks.state.\"k\"]\ntrusted_hash = \"sha256:deadbeef\"\n"
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := client.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	assertCodexApprovalBeforeForeignTables(t, content)

	var out2 bytes.Buffer
	if _, err := client.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out2, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("second client.RenderConfigs: %v\n%s", err, out2.String())
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw2) != content {
		t.Errorf("second run not byte-identical:\nfirst:\n%s\nsecond:\n%s", content, raw2)
	}
}

func assertCodexApprovalBeforeForeignTables(t *testing.T, content string) {
	t.Helper()
	firstTable := strings.Index(content, "[")
	if firstTable < 0 {
		t.Fatalf("no table header found at all:\n%s", content)
	}
	approvalIdx := strings.Index(content, "approval_policy")
	sandboxIdx := strings.Index(content, "sandbox_mode")
	if approvalIdx < 0 || sandboxIdx < 0 {
		t.Fatalf("approval_policy/sandbox_mode not rendered:\n%s", content)
	}
	// Before the first "[" means top-level: TOML has no way for a key there to belong to any table.
	if approvalIdx > firstTable || sandboxIdx > firstTable {
		t.Errorf("approval_policy/sandbox_mode (at %d, %d) must precede the first [table] header (at %d):\n%s",
			approvalIdx, sandboxIdx, firstTable, content)
	}
	if !strings.Contains(content, "[projects.\"/x\"]") || !strings.Contains(content, "trust_level = \"trusted\"") {
		t.Errorf("foreign [projects] table was dropped or altered:\n%s", content)
	}
	if !strings.Contains(content, "[hooks.state.\"k\"]") || !strings.Contains(content, "trusted_hash = \"sha256:deadbeef\"") {
		t.Errorf("foreign [hooks.state] table was dropped or altered:\n%s", content)
	}
}

// topLevelKeyCount counts "key = " assignments in content that land before the first top-level
// [table] header (TOML has no way for such a key to belong to any table), skipping bracket-nested
// and multiline-string content the same way agentconfig.InsertTopLevelTOML does.
func topLevelKeyCount(content, key string) int {
	count := 0
	for _, line := range strings.SplitAfter(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			break
		}
		if strings.HasPrefix(trimmed, key+" ") {
			count++
		}
	}
	return count
}

// The managed banner's approval_policy/sandbox_mode are the vendor spec's top-level keys; a
// pre-existing top-level key of the same name must be removed (not duplicated), while a same-named
// key inside an unrelated table ([profiles.x]) is left alone.
func TestCodexBannerReplacesExistingTopLevelPermissionKeys(t *testing.T) {
	home := rendertest.Scratch(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := "approval_policy = \"on-request\"\n\n[profiles.x]\napproval_policy = \"untrusted\"\n"
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := client.RenderConfigs(agentrender.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)

	if n := topLevelKeyCount(content, "approval_policy"); n != 1 {
		t.Fatalf("top-level approval_policy count = %d, want 1 (stale key must be removed, not duplicated):\n%s", n, content)
	}
	if !strings.Contains(content, "approval_policy = \"never\"") {
		t.Errorf("top-level approval_policy must be the banner's value (never):\n%s", content)
	}
	if !strings.Contains(content, "[profiles.x]") || !strings.Contains(content, "approval_policy = \"untrusted\"") {
		t.Errorf("profiles.x table's approval_policy must stay intact:\n%s", content)
	}
}
