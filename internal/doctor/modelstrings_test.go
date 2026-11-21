package doctor

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestPrefixedModels(t *testing.T) {
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"dev": {Provider: "databricks"}}}
	for i, model := range []string{"dev:private-model-value", "plain-model-id", "qwen3:30b"} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			vaulttest.Use(t)
			t.Setenv("CODEX_MODEL", "dev:session-override")
			vaulttest.SetRecord(t, "codex", "model", model)
			vaulttest.SetRecord(t, "claude", "aliases.opus", model)
			vaulttest.Put(t, vault.Env, "PI_MODEL", model)
			vaulttest.Put(t, vault.Env, "API_KEY", "dev:must-stay-private")
			body := fmt.Sprintf(`{"env":{"ANTHROPIC_DEFAULT_OPUS_MODEL":%q,"ANTHROPIC_MODEL":%q,"API_KEY":"dev:must-stay-private"}}`, model, model)
			if err := os.WriteFile(settings.SettingsFile(), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(vault.Path())
			if err != nil {
				t.Fatal(err)
			}
			got := modelStringsCheck(reg)
			assertModelStringsRow(t, got, model != "dev:private-model-value")
			after, err := os.ReadFile(vault.Path())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("scan changed vault: %v", err)
			}
			stored, err := os.ReadFile(settings.SettingsFile())
			if err != nil || string(stored) != body {
				t.Fatalf("scan changed settings: %v", err)
			}
		})
	}
}

func assertModelStringsRow(t *testing.T, got check, wantOK bool) {
	t.Helper()
	if got.Name != "model strings" || got.OK != wantOK || got.Hard {
		t.Fatalf("row = %+v, want OK=%v and advisory", got, wantOK)
	}
	for _, hidden := range []string{"private-model-value", "must-stay-private", "API_KEY"} {
		if strings.Contains(got.Detail, hidden) {
			t.Fatalf("stored value or unrelated key leaked: %s", got.Detail)
		}
	}
	if wantOK {
		return
	}
	for _, want := range []string{
		"5 prefixed model(s)", "agent-records.codex.model", "agent-records.claude.aliases.opus", "vault:env.PI_MODEL",
		"env.ANTHROPIC_DEFAULT_OPUS_MODEL", "env.ANTHROPIC_MODEL",
		`--overlay '{"provider":"databricks","profile":"dev"}' --model <model>`,
	} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q missing %q", got.Detail, want)
		}
	}
}

func TestModelStringsScansOverlayAndDispatches(t *testing.T) {
	vaulttest.Use(t)
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"dev": {Provider: "databricks"}}}
	vaulttest.Put(t, vault.Env, "COWORKER_OVERLAY", `{"provider":"databricks","model":"dev:hidden-overlay-model"}`)
	for _, rec := range []agent.Record{
		{ID: "d-old", Time: time.Now().Add(-time.Hour), Request: agent.RunOptions{Model: "dev:opus"}},
		{ID: "d-new", Time: time.Now(), Request: agent.RunOptions{Model: "dev:sonnet"}},
		{ID: "d-ok", Time: time.Now(), Request: agent.RunOptions{Model: "opus"}},
	} {
		if err := rec.Save(); err != nil {
			t.Fatal(err)
		}
	}
	got := modelStringsCheck(reg)
	if got.OK || strings.Contains(got.Detail, "hidden-overlay-model") || strings.Contains(got.Detail, "dev:opus") {
		t.Fatalf("row = %+v", got)
	}
	for _, want := range []string{"vault:env.COWORKER_OVERLAY.model", "2 of the newest", "fails on --retry", "--overlay"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q missing %q", got.Detail, want)
		}
	}
}
