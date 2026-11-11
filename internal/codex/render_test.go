package codex

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	_ "github.com/edotau/claude-code/internal/gemini"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths"
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

func writeForeign(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

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
	writeForeign(t, codexPath, foreignCodex)
	// opencode's own config is never touched: foreign content must survive byte-for-byte.
	ocPath := filepath.Join(home, ".config", "opencode", "opencode.json")
	foreignOC := `{"provider": {"myown": {"npm": "whatever"}}, "someForeignKey": true}`
	writeForeign(t, ocPath, foreignOC)

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

// A user-set cli_auth_credentials_store outside the managed banner is kept verbatim, and the render
// reports it rather than silently overwriting a value the user chose for themselves.
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

// TestVendorBaselineSeededOnFirstWrite: with no prior baseline, the first write seeds one matching
// exactly what was written — today's behavior, now backed by an on-disk comparison basis for next time.
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

// TestVendorBaselineSeededWhenUnchanged: a file already current but with no baseline (installed before
// baselines existed) gets one, so a later hand edit is still detected.
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

// TestLostLineNonInteractiveOverwritesWithWarning: a hand edit inside the managed banner, re-rendered
// non-interactively (Confirm nil — matches launch/configure/make install), writes anyway and warns.
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

// TestConfirmFalseKeepsFile: an interactive caller's Confirm declining the overwrite leaves the file
// (and its hand edit) untouched.
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

// TestConfirmTrueWrites: an interactive caller's Confirm accepting the overwrite writes through,
// same as the non-interactive path, but by explicit choice rather than by default.
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
	writeForeign(t, codexPath, current)
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

// TestRevertRestoresNewestThenStepsBack: --revert restores the most recent version, consumes it
// (removes the version file), and a second --revert steps one version further back.
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

// tamperCodexConfig edits the managed block in place and returns the edited text.
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
	// Own login no longer fully "skips": the model_providers block is skipped, but approval/sandbox
	// keys (agentconfig, independent of login type) still render.
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
