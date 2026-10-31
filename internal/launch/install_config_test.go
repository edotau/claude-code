package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Install must re-render agent configs so a stale harness hook in codex's hooks.json is purged.
func TestInstallRendersAgentConfigs(t *testing.T) {
	hermetic(t, "")
	home := os.Getenv("HOME")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	hooksFile := filepath.Join(home, ".codex", "hooks.json")
	stale := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command",` +
		`"command":"HARNESS_HOOK_VENDOR=codex $HOME/.claude/bin/claude-code hook safety-scan"}]}]}}`
	if err := os.MkdirAll(filepath.Dir(hooksFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksFile, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	got := agentConfigsSummary(&out)
	if !strings.Contains(got, "configs rendered:") || !strings.Contains(got, "codex") {
		t.Fatalf("summary = %q (warnings: %s)", got, out.String())
	}
	b, err := os.ReadFile(hooksFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "HARNESS_HOOK_VENDOR=codex $HOME") {
		t.Errorf("stale entry survived:\n%s", b)
	}
	if again := agentConfigsSummary(&out); again != "configs unchanged" {
		t.Errorf("second run = %q, want unchanged", again)
	}
}

// TestInstallSettingsFreshRestoresThenMergesExisting: no settings.json restores the render whole; an
// existing file keeps its foreign keys through the merge.
func TestInstallSettingsFreshRestoresThenMergesExisting(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	want, err := settings.RenderLive()
	if err != nil {
		t.Fatal(err)
	}
	if err := installSettings(&bytes.Buffer{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SettingsFile()); err == nil {
		t.Fatal("--dry-run created settings.json")
	}
	if err := installSettings(&bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(SettingsFile()); string(got) != string(want) {
		t.Fatalf("fresh install != render:\n%s", got)
	}
	merged := strings.Replace(string(want), "{\n", "{\n  \"zzForeign\": 1,\n", 1)
	if err := os.WriteFile(SettingsFile(), []byte(merged), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installSettings(&bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(SettingsFile()); !strings.Contains(string(got), "zzForeign") {
		t.Errorf("install over an existing file dropped a foreign key:\n%s", got)
	}
}

// TestDoctorSettingsDriftIsInfoNotFailure: changed keys are information; invalid JSON still fails.
func TestDoctorSettingsDriftIsInfoNotFailure(t *testing.T) {
	vaulttest.Use(t)
	if _, err := RestoreSettings(SettingsFile()); err != nil {
		t.Fatal(err)
	}
	if ok, detail := DoctorSettingsDrift(); !ok || detail != "clean" {
		t.Fatalf("after restore: ok=%v detail=%q, want clean", ok, detail)
	}
	if err := os.WriteFile(SettingsFile(), []byte(`{"model":"x","zzExtra":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, detail := DoctorSettingsDrift()
	if !ok || !strings.Contains(detail, "keys differ from your defaults — claude-code configure --restore") {
		t.Errorf("changed keys: ok=%v detail=%q, want info", ok, detail)
	}
	if err := os.WriteFile(SettingsFile(), []byte(`{nope`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, detail := DoctorSettingsDrift(); ok {
		t.Errorf("invalid JSON: ok=true detail=%q, want a failure", detail)
	}
}

// TestCheckSettingsDriftSeesWhatRestoreWouldRemove: an extra env var the merge would keep is drift,
// because --restore replaces the whole file.
func TestCheckSettingsDriftSeesWhatRestoreWouldRemove(t *testing.T) {
	vaulttest.Use(t)
	if _, err := RestoreSettings(SettingsFile()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(SettingsFile())
	edited := strings.Replace(string(raw), `"env": {`, `"env": {`+"\n    \"ZZ_EXTRA\": \"1\",", 1)
	if edited == string(raw) {
		t.Fatal("no env block to edit")
	}
	if err := os.WriteFile(SettingsFile(), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := CheckSettingsDrift()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Drifted() || !strings.Contains(strings.Join(d.Differs, ","), "env.ZZ_EXTRA") {
		t.Errorf("drift = %+v, want env.ZZ_EXTRA reported", d)
	}
}
