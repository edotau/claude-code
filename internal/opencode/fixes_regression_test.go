package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func regressionDatabricksProfile(t *testing.T, name string, menu []providers.OpencodeModel) *providers.Provider {
	t.Helper()
	vaulttest.Use(t)
	overlay := fmt.Sprintf(`{"providers":{"databricks":{%q:{"preset":"databricks","host":"https://workspace.example.test"}}}}`, name)
	vaulttest.Put(t, vault.Overlay, "providers", overlay)
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Row(name)
	if p == nil {
		t.Fatalf("missing test profile %q", name)
	}
	return p
}

func regressionGeneratedMenu(t *testing.T, p *providers.Provider, model string) []byte {
	t.Helper()
	env, _, err := opencodeDatabricks(p, "harness-"+p.Name, providers.ProtocolOpenAI, p.BaseURL, agentconfig.MenuWire(p, model), "", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range env {
		if body, ok := strings.CutPrefix(kv, opencodeConfigEnv+"="); ok {
			return []byte(body)
		}
	}
	t.Fatal("generated launch has no opencode config")
	return nil
}

func TestInteractiveLaunchRejectsPluginWriteFailure(t *testing.T) {
	menu := []providers.OpencodeModel{{ID: "glm-5-2", Label: "GLM 5.2"}}
	db := regressionDatabricksProfile(t, "databricks-test", menu)
	blocked := filepath.Join(paths.StateDir(), "opencode")
	if err := os.MkdirAll(filepath.Dir(blocked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*providers.Provider{{Name: "generic", Provider: "github"}, db} {
		t.Run(p.Name, func(t *testing.T) {
			env, argv, err := opencode(p, providers.ProtocolOpenAI, "https://workspace.example.test", agentconfig.MenuWire(p, "glm-5-2"), "", false, nil)
			if err == nil || !strings.Contains(err.Error(), "opencode plugin:") {
				t.Fatalf("interactive launch must report plugin write failure: env=%v argv=%v err=%v", env, argv, err)
			}
			if env != nil || argv != nil {
				t.Fatalf("failed launch returned usable config: env=%v argv=%v", env, argv)
			}
		})
	}
}

func TestParseMenuImportHyphenatedProfile(t *testing.T) {
	want := []providers.OpencodeModel{{ID: "glm-5-2", Label: "GLM 5.2", Responses: true}}
	p := regressionDatabricksProfile(t, "qa-prod", want)
	body := regressionGeneratedMenu(t, p, "glm-5-2")
	if !strings.Contains(string(body), `"harness-qa-prod-gpt"`) {
		t.Fatalf("fixture lacks hyphenated provider ID: %s", body)
	}
	got, err := ParseMenuImport(body)
	if err != nil {
		t.Fatalf("import generated hyphenated profile: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("imported menu = %+v, want %+v", got, want)
	}
}

func TestParseMenuImportPreservesExplicitRouting(t *testing.T) {
	menu := []providers.OpencodeModel{
		{ID: "glm-5-2", Label: "GLM 5.2", Responses: true},
		{ID: "gpt-6-1-sol", Label: "GPT 6.1 Sol", Responses: false},
	}
	p := regressionDatabricksProfile(t, "databricks-test", menu)
	got, err := ParseMenuImport(regressionGeneratedMenu(t, p, "glm-5-2"))
	if err != nil {
		t.Fatal(err)
	}
	want := []providers.OpencodeModel{menu[1], menu[0]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("import must retain explicit Responses and chat overrides: got %+v, want %+v", got, want)
	}
}

func TestOpencodePolicyArgvAutoPrompt(t *testing.T) {
	req := agent.RunOptions{Tools: true, Permissions: agent.Permissions{Deny: rules(t, "Read(.env)")}}
	argv := opencodeArgv("p/m", agent.Headless{Prompt: "--auto", Tools: true, Autonomous: false})
	before := slices.Clone(argv)
	got := opencodePolicyArgv(argv, req)
	want := append([]string{"run", "--auto"}, before[1:]...)
	if !slices.Equal(got, want) {
		t.Fatalf("prompt --auto must not suppress the policy flag: got %q, want %q", got, want)
	}
	if !slices.Equal(argv, before) {
		t.Fatalf("policy mutated input argv: got %q, want %q", argv, before)
	}
	if again := opencodePolicyArgv(got, req); !slices.Equal(again, got) {
		t.Fatalf("real --auto flag duplicated: %q", again)
	}
}

func regressionPluginTool(t *testing.T, tool, stdout string, mods []pluginModule) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakecc")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '" + stdout + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := renderPlugin(bin, mods)
	if err != nil {
		t.Fatal(err)
	}
	driver := fmt.Sprintf(`import { ClaudeCodeHarness } from "./plugin.mjs"
const h = await ClaudeCodeHarness({ directory: %q })
try {
  await h["tool.execute.before"]({ tool: %q, sessionID: "s1" }, { args: { filePath: "file.go", edits: [] } })
  console.log("RAN")
} catch (e) {
  console.log("BLOCKED " + e.message)
}
`, dir, tool)
	return runPluginNode(t, dir, body, driver, os.Environ())
}

func TestPluginMultieditRunsEditGuards(t *testing.T) {
	deny := `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"edit denied"}}`
	out := regressionPluginTool(t, "multiedit", deny, pluginModules())
	if strings.TrimSpace(out) != "BLOCKED edit denied" {
		t.Fatalf("multiedit must reach registered edit guards, got:\n%s", out)
	}
}

func TestPluginMalformedGuardOutput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stdout     string
		failClosed bool
		want       string
	}{
		{"closed", "{broken", true, "BLOCKED permission-gate did not complete (malformed output); blocked"},
		{"open", "{broken", false, "claude-code plugin: permission-gate did not complete (malformed output); allowed\nRAN"},
		{"silent allow", "", true, "RAN"},
		{"explicit allow", `{"hookSpecificOutput":{"permissionDecision":"allow"}}`, true, "RAN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mods := []pluginModule{{Name: "permission-gate", Matcher: "Edit", BudgetMs: 5000, FailClosed: tc.failClosed}}
			out := regressionPluginTool(t, "edit", tc.stdout, mods)
			if strings.TrimSpace(out) != tc.want {
				t.Fatalf("guard verdict = %q, want %q", out, tc.want)
			}
		})
	}
}
