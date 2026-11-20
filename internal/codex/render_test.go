package codex

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	_ "github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/models"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCodexCredStoreKeyIgnoresMultilineInstructions(t *testing.T) {
	for _, quote := range []string{`"""`, `'''`} {
		t.Run(quote, func(t *testing.T) {
			existing := "instructions = " + quote + "\n  cli_auth_credentials_store = \"file\"\n" + quote + "\n"
			want := "cli_auth_credentials_store = \"keyring\"\n"
			if got := codexCredStoreKey(existing, io.Discard); got != want {
				t.Fatalf("multiline instructions suppressed the keyring setting: got %q, want %q", got, want)
			}
		})
	}
}

// Approval/sandbox top-level keys land before the first [table] header and a second render is byte-identical.
func TestCodexApprovalKeysPrecedeExistingTables(t *testing.T) {
	home := agenttest.Scratch(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := "[projects.\"/x\"]\ntrust_level = \"trusted\"\n\n[hooks.state.\"k\"]\ntrusted_hash = \"sha256:deadbeef\"\n"
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	assertCodexApprovalBeforeForeignTables(t, content)

	var out2 bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out2, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("second agent.RenderConfigs: %v\n%s", err, out2.String())
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

// topLevelKeyCount counts "key = " assignments before the first top-level [table] header.
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

// The banner replaces a top-level approval_policy (no duplicate) and leaves one inside [profiles.x] alone.
func TestCodexBannerReplacesExistingTopLevelPermissionKeys(t *testing.T) {
	home := agenttest.Scratch(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := "approval_policy = \"on-request\"\n\n[profiles.x]\napproval_policy = \"untrusted\"\n"
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
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

const foreignCodex = "# a hand-written line\nsome_other_key = \"keep-me\"\n\n[projects.\"/x\"]\ntrust_level = \"trusted\"\n"

func assertForeignCodexKept(t *testing.T, got string) {
	t.Helper()
	if !strings.Contains(got, "some_other_key = \"keep-me\"") {
		t.Errorf("foreign codex line was dropped:\n%s", got)
	}
	if !strings.Contains(got, "[projects.\"/x\"]") || !strings.Contains(got, "trust_level = \"trusted\"") {
		t.Errorf("foreign [projects] table was dropped or altered:\n%s", got)
	}
	// The managed banner is inserted before the first pre-existing [table] header, not appended after it.
	bannerIdx := strings.Index(got, "# >>> claude-code:managed:begin >>>")
	tableIdx := strings.Index(got, "[projects.\"/x\"]")
	if bannerIdx < 0 {
		t.Fatalf("managed banner missing:\n%s", got)
	}
	if bannerIdx > tableIdx {
		t.Errorf("managed banner (%d) must precede the pre-existing [projects] table (%d):\n%s", bannerIdx, tableIdx, got)
	}
}

func assertCodexBackupKept(t *testing.T, codexPath string) {
	t.Helper()
	agenttest.AssertNotExist(t, "plaintext backup must not be written", codexPath+".backup")
	backup, found, err := LatestCodexBackup(codexPath)
	if err != nil || !found {
		t.Fatalf("encrypted backup missing: found=%v err=%v", found, err)
	}
	if string(backup.Data) != foreignCodex {
		t.Errorf("encrypted backup did not retain original bytes")
	}
}

func TestForeignKeysSurviveAndBackupIsWritten(t *testing.T) {
	home := agenttest.Scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	hosttest.WriteRel(t, "", codexPath, foreignCodex)
	// opencode's own config is never touched: foreign content must survive byte-for-byte.
	ocPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	foreignOC := `{"provider": {"myown": {"npm": "whatever"}}, "someForeignKey": true}`
	hosttest.WriteRel(t, "", ocPath, foreignOC)

	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex", "opencode"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}

	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	assertForeignCodexKept(t, string(got))
	assertCodexBackupKept(t, codexPath)

	ocGot, err := os.ReadFile(ocPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(ocGot) != foreignOC {
		t.Errorf("opencode's own config was touched: got %q, want untouched %q", ocGot, foreignOC)
	}
	agenttest.AssertExists(t, "overlay was not written to state/harness/opencode", filepath.Join(paths.StateDir(), "opencode", "opencode.json"))
}

// A user-set cli_auth_credentials_store outside the managed banner is kept verbatim and reported, never overwritten.
func TestCodexCredStoreKeyUserValueKept(t *testing.T) {
	home := agenttest.Scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "cli_auth_credentials_store = \"file\"\n"
	if err := os.WriteFile(codexPath, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `cli_auth_credentials_store = "file"`) {
		t.Errorf("user's cli_auth_credentials_store was overwritten:\n%s", got)
	}
	if strings.Contains(string(got), `cli_auth_credentials_store = "keyring"`) {
		t.Errorf("harness must not add its own key alongside the user's:\n%s", got)
	}
	if !strings.Contains(out.String(), "codex: cli_auth_credentials_store already set by the user; leaving it") {
		t.Errorf("expected the leaving-it note, got:\n%s", out.String())
	}
}

// TestVendorBaselineSeededOnFirstWrite
func TestVendorBaselineSeededOnFirstWrite(t *testing.T) {
	home := agenttest.Scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	base, err := loadCodexBaseline(codexPath)
	if err != nil {
		t.Fatalf("no baseline seeded: %v", err)
	}
	if !bytes.Equal(base, raw) {
		t.Errorf("seeded baseline = %q, want the written file %q", base, raw)
	}
}

// TestVendorBaselineSeededWhenUnchanged
func TestVendorBaselineSeededWhenUnchanged(t *testing.T) {
	home := agenttest.Scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	opts := func() agentconfig.RenderOptions {
		return agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}
	}
	if _, err := agent.RenderConfigs(opts()); err != nil {
		t.Fatal(err)
	}
	if err := vault.Update(func(tx *vault.Tx) error {
		for _, name := range tx.Names(vault.SettingsBackups) {
			if strings.HasPrefix(name, "codex-baseline/") {
				if err := tx.Put(vault.SettingsBackups, name, ""); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.RenderConfigs(opts()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(codexPath)
	base, err := loadCodexBaseline(codexPath)
	if err != nil {
		t.Fatalf("unchanged render seeded no baseline: %v", err)
	}
	if !bytes.Equal(base, raw) {
		t.Errorf("seeded baseline = %q, want the current file %q", base, raw)
	}
}

// TestLostLineNonInteractiveOverwritesWithWarning
func TestLostLineNonInteractiveOverwritesWithWarning(t *testing.T) {
	home := agenttest.Scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), `model = "gpt-5"`, `model = "gpt-5-custom"`, 1)
	if edited == string(raw) {
		t.Fatal("tamper anchor not found")
	}
	if err := os.WriteFile(codexPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hand-edited line") || !strings.Contains(out.String(), `model = "gpt-5-custom"`) {
		t.Errorf("expected a lost-line warning naming the edited line, got:\n%s", out.String())
	}
	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "gpt-5-custom") {
		t.Errorf("non-interactive must still overwrite the hand edit:\n%s", got)
	}
}

// TestConfirmFalseKeepsFile
func TestConfirmFalseKeepsFile(t *testing.T) {
	home := agenttest.Scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(codexPath)
	edited := strings.Replace(string(raw), `model = "gpt-5"`, `model = "gpt-5-custom"`, 1)
	if err := os.WriteFile(codexPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	opts := agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}
	opts.Confirm = func(path string, lost []string) bool { return false }
	if _, err := agent.RenderConfigs(opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "kept") {
		t.Errorf("expected a kept-your-edits note, got:\n%s", out.String())
	}
	got, _ := os.ReadFile(codexPath)
	if !strings.Contains(string(got), "gpt-5-custom") {
		t.Errorf("Confirm=false must keep the hand edit:\n%s", got)
	}
}

// TestConfirmTrueWrites
func TestConfirmTrueWrites(t *testing.T) {
	home := agenttest.Scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(codexPath)
	edited := strings.Replace(string(raw), `model = "gpt-5"`, `model = "gpt-5-custom"`, 1)
	if err := os.WriteFile(codexPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}
	opts.Confirm = func(path string, lost []string) bool { return true }
	if _, err := agent.RenderConfigs(opts); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(codexPath)
	if strings.Contains(string(got), "gpt-5-custom") {
		t.Errorf("Confirm=true must overwrite the hand edit:\n%s", got)
	}
}

// TestCheckListsLostLinesWritesNothing: --check reports the lost line without touching the file.
func TestCheckListsLostLinesWritesNothing(t *testing.T) {
	home := agenttest.Scratch(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &bytes.Buffer{}, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(codexPath)
	edited := strings.Replace(string(raw), `model = "gpt-5"`, `model = "gpt-5-custom"`, 1)
	if err := os.WriteFile(codexPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Check: true, Agents: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `model = "gpt-5-custom"`) {
		t.Errorf("--check must list the lost line, got:\n%s", out.String())
	}
	got, _ := os.ReadFile(codexPath)
	if !strings.Contains(string(got), "gpt-5-custom") {
		t.Errorf("--check must not write:\n%s", got)
	}
}

func revertCodex(t *testing.T, home, codexPath string) string {
	t.Helper()
	if err := agent.RevertConfigs(agentconfig.RenderOptions{Home: home, Agents: []string{"codex"}, Out: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// seedCodexBackups writes current to codexPath and backs up each version in order; returns the newest backup.
func seedCodexBackups(t *testing.T, codexPath, current string, versions ...string) codexConfigBackup {
	t.Helper()
	hosttest.WriteRel(t, "", codexPath, current)
	for _, v := range versions {
		if err := CodexBackup(codexPath, []byte(v), true); err != nil {
			t.Fatal(err)
		}
	}
	newest, found, err := LatestCodexBackup(codexPath)
	if err != nil || !found {
		t.Fatalf("backup missing: %v", err)
	}
	return newest
}

// TestRevertRestoresNewestThenStepsBack
func TestRevertRestoresNewestThenStepsBack(t *testing.T) {
	vaulttest.Use(t)
	home := t.TempDir()
	codexPath := filepath.Join(home, ".codex", "config.toml")
	older, newer := "older\n", "newer\n"
	newest := seedCodexBackups(t, codexPath, "current\n", older, newer)

	if got := revertCodex(t, home, codexPath); got != newer {
		t.Fatalf("first revert: got %q, want newest version %q", got, newer)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		if _, found := tx.Get(vault.SettingsBackups, newest.Name); found {
			t.Error("restored snapshot was not consumed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if got := revertCodex(t, home, codexPath); got != older {
		t.Fatalf("second revert: got %q, want next-older version %q", got, older)
	}
}

func tamperCodexConfig(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "model_provider", "model_provider_TAMPERED", 1)
	if edited == string(raw) {
		t.Fatal("tamper replace found no anchor")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	return edited
}

func TestCheckExitsOnDriftAndWritesNothing(t *testing.T) {
	home := agenttest.Scratch(t)
	var out bytes.Buffer
	if drift, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Check: true, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v", err)
	} else if !drift {
		t.Fatal("expected drift=true against an empty home")
	}
	path := filepath.Join(home, ".codex", "config.toml")
	agenttest.AssertNotExist(t, "--check must write nothing", path)

	// Write it for real, then hand-edit the managed block and confirm --check reports drift again.
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	edited := tamperCodexConfig(t, path)
	var out2 bytes.Buffer
	drift, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out2, Check: true, Agents: []string{"codex"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !drift {
		t.Fatal("expected drift=true after a hand edit")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != edited {
		t.Fatal("--check must not write")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	home := agenttest.Scratch(t)
	var out bytes.Buffer
	drift, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, DryRun: true, Agents: []string{"codex", "opencode", "gemini"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !drift {
		t.Fatal("expected drift=true (would write) on an empty home")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Fatalf("--dry-run must write nothing, stat err = %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("expected --dry-run to print the rendered blocks")
	}
}

func TestCodexOnOwnLoginSkipsInsteadOfFailing(t *testing.T) {
	home := agenttest.Scratch(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"default": "chatgpt"}`)
	codexHome := os.Getenv("CODEX_HOME")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"id_token":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	// Own login no longer fully "skips"
	if !strings.Contains(out.String(), "codex: chatgpt runs on codex's own login; writing approval/sandbox keys only") {
		t.Fatalf("want the own-login codex report line, got:\n%s", out.String())
	}
	// Hook trust entries land regardless of login type; only the model_providers block is skipped.
	raw, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if strings.Contains(string(raw), "model_providers.claude-code") {
		t.Fatalf("own-login codex must not get a model_providers block:\n%s", raw)
	}
	if !strings.Contains(string(raw), "[hooks.state.") {
		t.Fatalf("own-login codex should still get hook trust entries:\n%s", raw)
	}
	// Top-level approval/sandbox keys are agentconfig's, independent of login type: written even here.
	if !strings.Contains(string(raw), "approval_policy") || !strings.Contains(string(raw), "sandbox_mode") {
		t.Fatalf("own-login codex should still get approval_policy/sandbox_mode:\n%s", raw)
	}
}

func codexDbxFixture() (*providers.Registry, providers.Target) {
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks, BaseURL: "https://dbx.test",
		Models: map[string]string{models.Opus: "databricks-claude-opus-5-5"},
		Routes: map[string]string{providers.ProtocolResponses: "/responses", providers.ProtocolOpenAI: "/chat", providers.ProtocolAnthropic: "/anthropic"}}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"dev": p}}
	return reg, providers.Target{Provider: p, Model: "databricks-claude-opus-5-5"}
}

func TestCodexRenderModelPicksResponsesModelWithoutPin(t *testing.T) {
	vaulttest.Use(t)
	reg, target := codexDbxFixture()
	var out bytes.Buffer
	got := codexRenderModel(context.Background(), reg, target, &out)
	if want := providers.FirstResponsesModel(target.Provider); want == "" || got.Model != want {
		t.Fatalf("model = %q, want the profile's Responses model %q", got.Model, want)
	}
	if !strings.Contains(out.String(), "codex: wrote model "+got.Model+" on profile dev") {
		t.Fatalf("missing notice, got %q", out.String())
	}
}

func TestCodexRenderModelHonoursPin(t *testing.T) {
	vaulttest.Use(t)
	reg, target := codexDbxFixture()
	if err := vault.SetAgentRecord("codex", vault.AgentRecord{Model: "databricks-gpt-6-1-sol"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	got := codexRenderModel(context.Background(), reg, target, &out)
	if got.Model != "databricks-gpt-6-1-sol" || !strings.Contains(out.String(), "the codex model pin") {
		t.Fatalf("model = %q, out = %q, want the pin", got.Model, out.String())
	}
}

func TestCodexRenderModelLeavesOtherRowsAlone(t *testing.T) {
	vaulttest.Use(t)
	reg, target := codexDbxFixture()
	target.Provider.Preset = ""
	var out bytes.Buffer
	if got := codexRenderModel(context.Background(), reg, target, &out); got.Model != target.Model || out.Len() != 0 {
		t.Fatalf("non-Databricks target changed: %q %q", got.Model, out.String())
	}
}

const routeRows = `{"default":"ultra","providers":{"databricks":{` +
	`"dev":{"preset":"databricks","host":"https://zq1.example.test","auth":{"type":"key","command":"echo zq"}},` +
	`"qa":{"preset":"databricks","host":"https://zq2.example.test","auth":{"type":"key","command":"echo zq"}}}}}`

// routeFixture is a hermetic home whose session profile (ultra) cannot serve codex, beside two Databricks rows.
func routeFixture(t *testing.T, route string) (string, *providers.Registry, providers.Selection) {
	t.Helper()
	home := agenttest.Scratch(t)
	oldMint := agent.MintSession
	agent.MintSession = func(context.Context, []string, int) (string, error) { return "zqtoken", nil }
	t.Cleanup(func() { agent.MintSession = oldMint })
	vaulttest.Put(t, vault.Overlay, "providers", routeRows)
	rec := vault.AgentRecord{}
	if route != "" {
		rec.Extras = map[string]string{extraConfigRoute: route}
	}
	if err := vault.SetAgentRecord("codex", rec); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	return home, reg, sel
}

func TestCodexConfigRouteWritesDatabricksBlockOnAnthropicSession(t *testing.T) {
	_, reg, sel := routeFixture(t, "qa")
	var out bytes.Buffer
	got, err := codexProviderConfig(context.Background(), reg, sel, "", &out)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`model_provider = "claude-code"`, "[model_providers.claude-code]",
		`name = "qa"`, "timeout_ms = 100000", "refresh_interval_ms = 300000",
		"stream_idle_timeout_ms = " + codexStreamIdleMS} {
		if !strings.Contains(got, want) {
			t.Errorf("block lacks %q:\n%s\n%s", want, got, out.String())
		}
	}
	if m := regexp.MustCompile(`command = "([^"]+)"`).FindStringSubmatch(got); m == nil || !filepath.IsAbs(m[1]) {
		t.Errorf("helper command must be an absolute path, got %v", m)
	}
}

func TestCodexConfigRouteDatabricksPicksPoolProfile(t *testing.T) {
	_, reg, sel := routeFixture(t, "databricks")
	var out bytes.Buffer
	got, err := codexProviderConfig(context.Background(), reg, sel, "", &out)
	if err != nil || !strings.Contains(got, "[model_providers.claude-code]") || !strings.Contains(out.String(), "config_route databricks") {
		t.Fatalf("pool route: err %v\n%s\n%s", err, got, out.String())
	}
}

func TestCodexConfigRouteUnsetKeepsSessionResult(t *testing.T) {
	for _, route := range []string{"", "session"} {
		_, reg, sel := routeFixture(t, route)
		var out bytes.Buffer
		got, err := codexProviderConfig(context.Background(), reg, sel, "", &out)
		if err != nil || strings.Contains(got, "model_providers") || !strings.Contains(got, "approval_policy") ||
			!strings.Contains(out.String(), "runs on codex's own login; writing approval/sandbox keys only") {
			t.Errorf("route %q: err %v, want approval keys and no provider block:\n%s\n%s", route, err, got, out.String())
		}
	}
}

func TestCodexConfigRouteBadValueWritesNothing(t *testing.T) {
	home, _, _ := routeFixture(t, "nosuch")
	var out bytes.Buffer
	_, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}})
	if err == nil || !strings.Contains(err.Error(), "want one of session, databricks, dev, qa") {
		t.Fatalf("err = %v, want the valid values named", err)
	}
	agenttest.AssertNotExist(t, "a bad config_route must write nothing", filepath.Join(home, ".codex"))
}

func TestCodexLaunchAuthKeepsShortTimeout(t *testing.T) {
	_, reg, sel := routeFixture(t, "qa")
	target := providers.Target{Provider: reg.Providers["qa"], Model: sel.Aliases[models.Opus].Model}
	target = codexRenderModel(context.Background(), reg, target, io.Discard)
	_, argv, err := codexSchema().Configure(context.Background(), target)
	if err != nil || !strings.Contains(strings.Join(argv, " "), "timeout_ms = 15000") {
		t.Fatalf("launch argv must keep timeout_ms = 15000: %v\n%v", err, argv)
	}
}

func TestCodexRestartNoticeOnlyOnRouteChange(t *testing.T) {
	home, _, _ := routeFixture(t, "qa")
	render := func() string {
		var out bytes.Buffer
		if _, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex"}}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	const notice = "restart a running ChatGPT app or Codex app-server"
	if first := render(); !strings.Contains(first, notice) {
		t.Fatalf("first render changes the provider, want the restart notice:\n%s", first)
	}
	if second := render(); strings.Contains(second, notice) {
		t.Fatalf("unchanged file must not print the notice:\n%s", second)
	}
}
