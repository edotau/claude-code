package codex

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCodexOwnModelSkipsMultilineValues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	raw := "notes = \"\"\"\nmodel = \"fake\"\n\"\"\"\nlist = [\n  \"a\",\n]\nmodel = \"real\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexOwnModel(); got != "real" {
		t.Errorf("got %q, want real", got)
	}
}

// Each record extra reaches config.toml; the record beats the legacy overlay, which still fills what it leaves unset.
func TestCodexConfigFromRecordExtras(t *testing.T) {
	vaulttest.Use(t)
	if err := SaveCodexOverlay(map[string]string{codexEffortKey: "medium", extraVerbosity: "high"}); err != nil {
		t.Fatal(err)
	}
	putCodexRecord(t, vault.AgentRecord{Effort: "max", Extras: map[string]string{
		extraApprovalPolicy: "on-request", extraSandboxMode: "workspace-write", extraVerbosity: "low"}})
	keys, err := codexPermissionKeys("", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	prefs, err := LoadCodexPreferences()
	if err != nil {
		t.Fatal(err)
	}
	block, err := patchCodexSettings("", prefs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`approval_policy = "on-request"`, `sandbox_mode = "workspace-write"`} {
		if !strings.Contains(keys, want) {
			t.Errorf("managed keys %q lack %s", keys, want)
		}
	}
	for _, want := range []string{`model_verbosity = "low"`, `model_reasoning_effort = "xhigh"`} {
		if !strings.Contains(block, want) {
			t.Errorf("settings block %q lacks %s", block, want)
		}
	}
	putCodexRecord(t, vault.AgentRecord{})
	if prefs, err = LoadCodexPreferences(); err != nil || prefs[codexEffortKey] != "medium" || prefs[extraVerbosity] != "high" {
		t.Errorf("overlay-only preferences = %v (%v), want the overlay's", prefs, err)
	}
}

// A preferences save lands in the codex record and empties the legacy overlay; an untouched max effort stays max.
func TestUpdateCodexPreferencesWritesRecord(t *testing.T) {
	vaulttest.Use(t)
	if err := SaveCodexOverlay(map[string]string{extraVerbosity: "high"}); err != nil {
		t.Fatal(err)
	}
	putCodexRecord(t, vault.AgentRecord{Effort: "max", Profile: "chatgpt"})
	if err := UpdateCodexPreferences(func(p map[string]string) map[string]string { return p }); err != nil {
		t.Fatal(err)
	}
	rec := vault.GetAgentRecord("codex")
	if rec.Effort != "max" || rec.Profile != "chatgpt" || rec.Extras[extraVerbosity] != "high" {
		t.Errorf("record after save = %+v, want effort max, profile kept, verbosity high moved in", rec)
	}
	if ov, err := LoadCodexOverlay(); err != nil || len(ov) != 0 {
		t.Errorf("legacy overlay after save = %v (%v), want empty", ov, err)
	}
	if err := UpdateCodexPreferences(func(p map[string]string) map[string]string {
		delete(p, codexEffortKey)
		p[extraVerbosity] = "low"
		return p
	}); err != nil {
		t.Fatal(err)
	}
	if rec = vault.GetAgentRecord("codex"); rec.Effort != "" || rec.Extras[extraVerbosity] != "low" {
		t.Errorf("record after edit = %+v, want effort cleared and verbosity low", rec)
	}
}
