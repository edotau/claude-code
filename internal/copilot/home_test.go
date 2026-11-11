package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agentconfig"
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/gemini"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// CopilotJSON renders exactly the native copilot specs on events Copilot supports, each as one command.
func TestCopilotJSONRendersNativeSpecs(t *testing.T) {
	body, err := CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Bash string `json:"bash"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for event, entries := range doc.Hooks {
		for _, e := range entries {
			got[event+"|"+strings.TrimSuffix(e.Bash, " || true")] = true
		}
	}
	want := map[string]bool{}
	for _, s := range settings.SpecsFor("copilot", "native") {
		if event := agentEvents[s.Event]; event != "" {
			want[event+"|"+s.Command()] = true
		}
	}
	if len(want) == 0 || len(got) != len(want) {
		t.Fatalf("rendered %d commands, want %d: %v", len(got), len(want), got)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("missing %q", k)
		}
	}
}

// Stop and SessionEnd map to Copilot's agentStop and sessionEnd, advisory (never denying).
func TestCopilotJSONMemoryEvents(t *testing.T) {
	body, err := CopilotJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Bash string `json:"bash"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	for event, modules := range map[string][]string{"agentStop": {"memory-update", "format-stop"}, "sessionEnd": {"memory-end"}} {
		entries := doc.Hooks[event]
		if len(entries) != len(modules) {
			t.Fatalf("%s = %+v, want advisory %v", event, entries, modules)
		}
		for i, module := range modules {
			if !strings.Contains(entries[i].Bash, module) || !strings.HasSuffix(entries[i].Bash, " || true") {
				t.Errorf("%s[%d] = %q, want advisory %s", event, i, entries[i].Bash, module)
			}
		}
	}

}

// An unreadable settings.json (here a directory) must be skipped, not rewritten from an empty map.
func TestRenderCopilotSettingsUnreadableIsSkipped(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	path := filepath.Join(home, ".copilot", "settings.json")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	changed, err := renderCopilotSettings(home, agentconfig.RenderOptions{}, &out)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v, want false,nil", changed, err)
	}
	if !strings.Contains(out.String(), "cannot read") {
		t.Fatalf("output %q lacks the cannot-read skip note", out.String())
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Fatalf("path was replaced: %v", err)
	}
}

// renderCopilotSettings merges defaultPermissionMode into ~/.copilot/settings.json, keeping every other key.
func TestCopilotSettingsMergesDefaultPermissionModeKeepingOtherKeys(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	path := settingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"theme":"default","allowedUrls":["https://example.com"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var plan agentconfig.RenderPlan
	if _, err := renderCopilotSettings(home, agentconfig.RenderOptions{}.Stage(&plan), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("settings.json not valid JSON: %v\n%s", err, raw)
	}
	if got["defaultPermissionMode"] != "allow-all" {
		t.Errorf("defaultPermissionMode = %v, want allow-all: %s", got["defaultPermissionMode"], raw)
	}
	urls, ok := got["allowedUrls"].([]any)
	if got["theme"] != "default" || !ok || len(urls) != 1 || urls[0] != "https://example.com" {
		t.Errorf("foreign keys dropped or altered: %s", raw)
	}
}

// A registry home already holding a GitHub login is refused by seedCopilotHome; an empty login is allowed.
func TestSeedCopilotHomeRefusesGitHubLogin(t *testing.T) {
	logins := []string{
		`{"lastLoggedInUser":{"host":"github.com","login":"octocat"}}`,
		`{"loggedInUsers":[{"login":"octocat"}]}`,
		`{"copilotTokens":{"github.com:octocat":"gho_x"}}`,
	}
	for _, body := range logins {
		home, xdg := filepath.Join(t.TempDir(), "home"), filepath.Join(t.TempDir(), "xdg")
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := seedCopilotHome(t.TempDir(), home, xdg); err == nil || !strings.Contains(err.Error(), "GitHub login") {
			t.Errorf("config.json %s: err = %v, want a GitHub-login refusal", body, err)
		}
	}
	empty := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(empty, []byte(`{"loggedInUsers":[],"lastLoggedInUser":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkNoGitHubLogin(empty); err != nil {
		t.Errorf("empty login fields refused: %v", err)
	}
}

func setGOOS(t *testing.T, goos string) {
	t.Helper()
	orig := copilotGOOS
	copilotGOOS = goos
	t.Cleanup(func() { copilotGOOS = orig })
}

// macOS is refused before any home is seeded and before the registry launch discovers models.
func TestRegistryRefusedOnMacOS(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	setGOOS(t, "darwin")
	if _, err := PrepareCopilotHome(false); !errors.Is(err, errCopilotRegistryDarwin) {
		t.Errorf("PrepareCopilotHome err = %v, want the macOS refusal", err)
	}
	if _, err := os.Stat(copilotRegistryHome()); !os.IsNotExist(err) {
		t.Errorf("registry home was created on macOS: %v", err)
	}
	orig := discoverModels
	discoverModels = func(context.Context, *providers.Registry, *providers.Provider) ([]CopilotEndpoint, error) {
		t.Fatal("discovery ran on macOS")
		return nil, nil
	}
	t.Cleanup(func() { discoverModels = orig })
	_, err := ConfigureCopilotRegistry(context.Background(), nil, providers.Target{Provider: &providers.Provider{Name: "dev"}}, nil, true)
	if !errors.Is(err, errCopilotRegistryDarwin) {
		t.Errorf("ConfigureCopilotRegistry err = %v, want the macOS refusal", err)
	}
}

// The prepared home is 0700, carries settings.json (0600, no model) and skills, never the source's GitHub login.
func TestPrepareCopilotHomeSeedsLoginFree(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	setGOOS(t, "linux")
	seedSourceHome(t)
	ph, err := PrepareCopilotHome(false)
	if err != nil {
		t.Fatal(err)
	}
	home := copilotRegistryHome()
	if !slices.Contains(ph.Env, "COPILOT_HOME="+home) || !slices.Contains(ph.Unset, "GH_TOKEN") {
		t.Errorf("env = %v unset = %v, want the private COPILOT_HOME and GH_TOKEN stripped", ph.Env, ph.Unset)
	}
	assertPerm(t, home, 0o700)
	assertPerm(t, copilotRegistryXDG(), 0o700)
	assertPerm(t, filepath.Join(home, "settings.json"), 0o600)
	if b, _ := os.ReadFile(filepath.Join(home, "settings.json")); strings.Contains(string(b), "model") || !strings.Contains(string(b), "dark") {
		t.Errorf("settings.json = %s, want theme kept and model dropped", b)
	}
	assertSeedLinks(t, home)
}

// assertSeedLinks checks home links the source's skills but not its config.json (the GitHub login).
func assertSeedLinks(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(home, "config.json")); !os.IsNotExist(err) {
		t.Errorf("config.json (the GitHub login) was carried into the private home: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(home, "skills")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("skills not linked from the real home: %v", err)
	}
}

// seedSourceHome makes COPILOT_HOME a real home with settings.json (a model key), a GitHub login and a skills dir.
func seedSourceHome(t *testing.T) {
	t.Helper()
	src := t.TempDir()
	t.Setenv("COPILOT_HOME", src)
	for name, body := range map[string]string{
		"settings.json": `{"model":"gpt-5","theme":"dark"}`,
		"config.json":   `{"lastLoggedInUser":{"login":"octocat"}}`,
	} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(src, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
		t.Errorf("%s: err %v, want mode %o", path, err, want)
	}
}

// A stored record's extras.defaultPermissionMode wins over the built-in allow-all in settings.json.
func TestCopilotSettingsReadsRecordExtras(t *testing.T) {
	vaulttest.UseFreshKey(t)
	if err := vault.SetAgentRecord("copilot", vault.AgentRecord{Extras: map[string]string{extraDefaultPermissionMode: "plan"}}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	var plan agentconfig.RenderPlan
	if _, err := renderCopilotSettings(home, agentconfig.RenderOptions{}.Stage(&plan), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(settingsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil || got["defaultPermissionMode"] != "plan" {
		t.Errorf("settings.json = %s (%v), want defaultPermissionMode plan", raw, err)
	}
}

// The macOS refusal survives the fallback notice's cap with its --direct hint intact, once the dispatch wraps it.
func TestRegistryRefusalKeepsDirectHintInFallbackReason(t *testing.T) {
	setGOOS(t, "darwin")
	wrapped := &retry.Error{Kind: retry.KindFatal, Agent: "copilot", Err: checkCopilotRegistryOS()}
	if got := agent.FallbackReason(wrapped); !strings.HasSuffix(got, "use --direct") {
		t.Errorf("FallbackReason = %q, want it to end with the --direct hint", got)
	}
}

func TestRunMalformedCopilotHooksWritesNothing(t *testing.T) {
	home := agenttest.Scratch(t)
	hooksPath := filepath.Join(home, ".copilot", "hooks", "claude-code-harness.json")
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err := agent.RenderConfigs(agentconfig.RenderOptions{Home: home, ForceHomes: true, Out: &out, Agents: []string{"codex", "opencode", "gemini", "copilot"}})
	if err == nil || !strings.Contains(err.Error(), "copilot-hooks: existing file:") {
		t.Fatalf("expected malformed Copilot hooks error, got %v\n%s", err, out.String())
	}
	for _, rel := range []string{
		".codex/config.toml", ".codex/hooks.json", ".gemini/antigravity-cli/settings.json", ".copilot/settings.json",
	} {
		agenttest.AssertNotExist(t, "must not be written after a render error", filepath.Join(home, rel))
	}
	// opencode's staged overlay write (ahead of copilot's in DefaultAgents order) must not apply either.
	for _, rel := range []string{"opencode.json", "tui.json"} {
		agenttest.AssertNotExist(t, "opencode overlay must not be written after a later render error", filepath.Join(paths.StateDir(), "opencode", rel))
	}
	if strings.Contains(out.String(), ": wrote ") || strings.Contains(out.String(), ": moved ") {
		t.Errorf("reported unapplied writes:\n%s", out.String())
	}
}
