package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const parityRows = `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com",` +
	`"auth":{"type":"none"},"models":{"opus":"databricks-claude-opus-5-5"}}},` +
	`"anthropic":{"alt":{"base_url":"https://alt.example.com","protocol":"anthropic","auth":{"type":"none"},"models":{"opus":"claude-opus-5-5"}}}}}`

func fakeBin(t *testing.T, name string) {
	t.Helper()
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.BinDir(), name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

var codexModelArg = regexp.MustCompile(`^model="(.*)"$`)

// launchModel is the model the interactive launch hands the CLI (--print-env plan): codex's -c model=, else COPILOT_MODEL.
func launchModel(t *testing.T, name string, o agent.LaunchOptions) string {
	t.Helper()
	o.PrintEnv = true
	plan, err := agent.LaunchPlan(context.Background(), name, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := plan.Set["COPILOT_MODEL"]; m != "" {
		return m
	}
	for i, a := range plan.Argv {
		if m := codexModelArg.FindStringSubmatch(a); m != nil && i > 0 && plan.Argv[i-1] == "-c" {
			return m[1]
		}
		if a == "--model" && i+1 < len(plan.Argv) {
			return plan.Argv[i+1]
		}
	}
	t.Fatalf("no model in %v", plan.Argv)
	return ""
}

// The interactive launch and the headless dispatch pick the same model for the same agent, profile, flag and pin.
func TestLaunchAndAgentAgreeOnModel(t *testing.T) {
	cases := []struct{ agent, pinKey, profile, flag string }{
		{"codex", providers.PinCodexModel, "dev", ""},
		{"codex", providers.PinCodexModel, "dev", "gpt-5.5"},
		{"copilot", providers.PinCopilotModel, "alt", ""},
		{"copilot", providers.PinCopilotModel, "alt", "claude-sonnet-5-5"},
	}
	for _, c := range cases {
		t.Run(c.agent+"/"+c.profile+"/"+c.flag, func(t *testing.T) {
			vaulttest.Use(t)
			vaulttest.Put(t, vault.Overlay, "providers", parityRows)
			fakeBin(t, c.agent)
			pin := "databricks-gpt-6-1-sol"
			if c.agent == "copilot" {
				pin = "claude-haiku-5-5"
			}
			if err := providers.SetPins(map[string]string{c.pinKey: pin}); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			tg, _, _, err := agent.VendorTargetNote(agent.RunOptions{Provider: c.profile, Model: c.flag}, c.agent)
			if err != nil {
				t.Fatal(err)
			}
			got := launchModel(t, c.agent, agent.LaunchOptions{Provider: c.profile, Model: c.flag})
			if got != tg.Model {
				t.Errorf("launch %q vs agent %q", got, tg.Model)
			}
			if c.flag == "" && !strings.Contains(got, pin) {
				t.Errorf("model %q ignores the pin %q", got, pin)
			}
		})
	}
}
