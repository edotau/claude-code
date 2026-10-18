package configure

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
)

// githubScratch isolates CLAUDE_CONFIG_DIR, HOME, XDG, PATH and every credential env var github.go
// reads, so a test never touches a real gh/copilot login or a real providers.json overlay.
func githubScratch(t *testing.T) (cfg string) {
	t.Helper()
	cfg, home, xdg := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, k := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN"} {
		t.Setenv(k, "")
	}
	return cfg
}

func readOverlay(t *testing.T, cfg string) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfg, "providers.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// WriteGithubCopilotModels merges opus/sonnet/haiku into providers.github-copilot.models in the user's
// overlay, preserving every other provider entry and root key byte-for-byte.
func TestWriteGithubCopilotModelsMergesIntoOverlay(t *testing.T) {
	cfg := githubScratch(t)
	existing := `{"default":"gw","providers":{"gw":{"kind":"anthropic","base_url":"https://gw.example.com"},"github-copilot":{"auth":{"type":"passthrough","login":"copilot"}}}}`
	if err := os.WriteFile(filepath.Join(cfg, "providers.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	picks := map[string]string{models.Opus: "gpt-6.1-sol", models.Sonnet: "claude-sonnet-5.5", models.Haiku: "claude-haiku-4.5"}
	if err := WriteGithubCopilotModels(picks, ""); err != nil {
		t.Fatalf("WriteGithubCopilotModels: %v", err)
	}
	doc := readOverlay(t, cfg)
	if string(doc["default"]) != `"gw"` {
		t.Errorf("default = %s, want it preserved", doc["default"])
	}
	var provs map[string]json.RawMessage
	if err := json.Unmarshal(doc["providers"], &provs); err != nil {
		t.Fatal(err)
	}
	if _, ok := provs["gw"]; !ok {
		t.Error("the unrelated gw provider entry was dropped")
	}
	var copilotEntry struct {
		Auth   json.RawMessage   `json:"auth"`
		Models map[string]string `json:"models"`
	}
	if err := json.Unmarshal(provs["github-copilot"], &copilotEntry); err != nil {
		t.Fatal(err)
	}
	if copilotEntry.Auth == nil {
		t.Error("github-copilot's pre-existing auth block was dropped")
	}
	if copilotEntry.Models[models.Opus] != "gpt-6.1-sol" || copilotEntry.Models[models.Sonnet] != "claude-sonnet-5.5" || copilotEntry.Models[models.Haiku] != "claude-haiku-4.5" {
		t.Errorf("models = %+v, want the three picks written", copilotEntry.Models)
	}
}

// A timestamped backup of the overlay's prior bytes is written before the new overlay replaces it.
func TestWriteGithubCopilotModelsWritesBackup(t *testing.T) {
	cfg := githubScratch(t)
	original := `{"providers":{"github-copilot":{"models":{"opus":"old-opus"}}}}`
	if err := os.WriteFile(filepath.Join(cfg, "providers.json"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	picks := map[string]string{models.Opus: "new-opus", models.Sonnet: "new-sonnet", models.Haiku: "new-haiku"}
	if err := WriteGithubCopilotModels(picks, ""); err != nil {
		t.Fatalf("WriteGithubCopilotModels: %v", err)
	}
	ents, err := os.ReadDir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var backup string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "providers.json.bak-") {
			backup = e.Name()
		}
	}
	if backup == "" {
		t.Fatalf("no providers.json.bak-* backup written; dir has %v", ents)
	}
	got, err := os.ReadFile(filepath.Join(cfg, backup))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Errorf("backup contents = %q, want the original overlay %q", got, original)
	}
}

// fallback="auto" is rejected outright: opencode's own catalog has no "auto" alias, so substituting
// it right back in would defeat the whole point of opencode_fallback. The overlay must stay untouched.
func TestWriteGithubCopilotModelsRejectsAutoFallback(t *testing.T) {
	cfg := githubScratch(t)
	picks := map[string]string{models.Opus: "gpt-6.1-sol", models.Sonnet: "claude-sonnet-5.5", models.Haiku: "auto"}
	err := WriteGithubCopilotModels(picks, "auto")
	if err == nil {
		t.Fatal("WriteGithubCopilotModels: want an error rejecting fallback=\"auto\"")
	}
	if !strings.Contains(err.Error(), "auto") {
		t.Errorf("error = %v, want it to name \"auto\"", err)
	}
	if _, err := os.Stat(filepath.Join(cfg, "providers.json")); err == nil {
		t.Error("a rejected fallback must not write the overlay at all")
	}
}

// fallback="" (no slot resolved to "auto") leaves any opencode_fallback the overlay already set alone.
func TestWriteGithubCopilotModelsEmptyFallbackLeavesExistingUnchanged(t *testing.T) {
	cfg := githubScratch(t)
	original := `{"providers":{"github-copilot":{"opencode_fallback":"mai-code-1.1-flash","models":{"opus":"old-opus"}}}}`
	if err := os.WriteFile(filepath.Join(cfg, "providers.json"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	picks := map[string]string{models.Opus: "claude-opus-5.5", models.Sonnet: "claude-sonnet-5.5", models.Haiku: "claude-haiku-4.5"}
	if err := WriteGithubCopilotModels(picks, ""); err != nil {
		t.Fatalf("WriteGithubCopilotModels: %v", err)
	}
	doc := readOverlay(t, cfg)
	var provs map[string]json.RawMessage
	if err := json.Unmarshal(doc["providers"], &provs); err != nil {
		t.Fatal(err)
	}
	var entry struct {
		OpencodeFallback string `json:"opencode_fallback"`
	}
	if err := json.Unmarshal(provs["github-copilot"], &entry); err != nil {
		t.Fatal(err)
	}
	if entry.OpencodeFallback != "mai-code-1.1-flash" {
		t.Errorf("opencode_fallback = %q, want the pre-existing value left untouched", entry.OpencodeFallback)
	}
}

// Every opus/sonnet/haiku slot must be a non-empty id; a missing one is rejected before anything is written.
func TestWriteGithubCopilotModelsRejectsEmptySlot(t *testing.T) {
	cfg := githubScratch(t)
	picks := map[string]string{models.Opus: "gpt-6.1-sol", models.Sonnet: "", models.Haiku: "claude-haiku-4.5"}
	if err := WriteGithubCopilotModels(picks, ""); err == nil {
		t.Fatal("WriteGithubCopilotModels: want an error for an empty sonnet slot")
	}
	if _, err := os.Stat(filepath.Join(cfg, "providers.json")); err == nil {
		t.Error("a rejected pick set must not write the overlay at all")
	}
}

// The piped `configure github` report prints only booleans, provider/model names and the fix commands —
// never a token-shaped string, even when a real-looking GitHub token sits in the environment.
func TestGithubSectionNonTTYOutputNamesNoSecret(t *testing.T) {
	githubScratch(t)
	t.Setenv("GH_TOKEN", "ghp_FAKEtokenlookingvalue1234567890")
	var out bytes.Buffer
	_ = Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "github")
	got := out.String()
	if strings.Contains(got, "ghp_FAKEtokenlookingvalue1234567890") {
		t.Errorf("github section leaked a credential:\n%s", got)
	}
	for _, want := range []string{"gh CLI", "copilot CLI", "opencode copilot", "route("} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}

// Confirms the "present" row never prints the credential itself, only the booleans/names github.go uses.
func TestGithubSectionReportsBooleansNotCredentials(t *testing.T) {
	githubScratch(t)
	t.Setenv("GH_TOKEN", "super-secret-gh-token-value")
	var out bytes.Buffer
	_ = Run(Options{Check: true, In: strings.NewReader(""), Out: &out}, "github")
	got := out.String()
	if !strings.Contains(got, "copilot CLI") || !strings.Contains(got, "present") {
		t.Errorf("want copilot CLI to read present via GH_TOKEN:\n%s", got)
	}
	if strings.Contains(got, "super-secret-gh-token-value") {
		t.Errorf("leaked GH_TOKEN value:\n%s", got)
	}
}
