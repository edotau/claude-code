package copilot

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agentconfig/render"
	"github.com/edotau/claude-code/internal/settings"
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
	changed, err := renderCopilotSettings(home, render.RenderOptions{}, &out)
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
