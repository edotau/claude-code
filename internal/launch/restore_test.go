package launch

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
