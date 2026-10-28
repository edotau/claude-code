package configure

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// githubScratch isolates CLAUDE_CONFIG_DIR, HOME, XDG, PATH and every credential env var github.go
// reads, so a test never touches a real gh/copilot login or a real vault overlay.
func githubScratch(t *testing.T) {
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
}

func TestRunGithubLoginPrintsVendorBinError(t *testing.T) {
	githubScratch(t)
	_, err := paths.VendorBin("copilot")
	if err == nil {
		t.Fatal("expected missing vendor")
	}
	var out bytes.Buffer
	runGithubLogin(&dialog{out: &out}, []string{"copilot", "login"}, "copilot")
	if want := "copilot: " + err.Error() + "\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

// flatOverlayRows flattens the stored nested overlay {provider:{profile:row}} to {profile:row}.
func flatOverlayRows(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var nested map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		t.Fatal(err)
	}
	flat := map[string]json.RawMessage{}
	for _, rows := range nested {
		for profile, row := range rows {
			flat[profile] = row
		}
	}
	return flat
}

func readOverlay(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := providers.OverlayDoc()
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// WriteGithubCopilotModels merges opus/sonnet/haiku into providers.github.personal.models in the user's
// overlay, preserving every other provider entry and root key byte-for-byte.
func TestWriteGithubCopilotModelsMergesIntoOverlay(t *testing.T) {
	githubScratch(t)
	existing := `{"default":"gw","providers":{"anthropic":{"gw":{"protocol":"anthropic","base_url":"https://gw.example.com","auth":{"type":"none"}}},"github":{"personal":{"protocol":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", existing)
	picks := map[string]string{models.Opus: "gpt-6.1-sol", models.Sonnet: "claude-sonnet-5.5", models.Haiku: "claude-haiku-4.5"}
	if err := WriteGithubCopilotModels(picks, ""); err != nil {
		t.Fatalf("WriteGithubCopilotModels: %v", err)
	}
	doc := readOverlay(t)
	if string(doc["default"]) != `"gw"` {
		t.Errorf("default = %s, want it preserved", doc["default"])
	}
	provs := flatOverlayRows(t, doc["providers"])
	if _, ok := provs["gw"]; !ok {
		t.Error("the unrelated gw provider entry was dropped")
	}
	var copilotEntry struct {
		Auth   json.RawMessage   `json:"auth"`
		Models map[string]string `json:"models"`
	}
	if err := json.Unmarshal(provs["personal"], &copilotEntry); err != nil {
		t.Fatal(err)
	}
	if copilotEntry.Auth == nil {
		t.Error("personal's pre-existing auth block was dropped")
	}
	if copilotEntry.Models[models.Opus] != "gpt-6.1-sol" || copilotEntry.Models[models.Sonnet] != "claude-sonnet-5.5" || copilotEntry.Models[models.Haiku] != "claude-haiku-4.5" {
		t.Errorf("models = %+v, want the three picks written", copilotEntry.Models)
	}
}

// The overlay's prior bytes are retained in the vault when the new overlay replaces it.
func TestWriteGithubCopilotModelsKeepsPreviousOverlay(t *testing.T) {
	githubScratch(t)
	original := `{"providers":{"github":{"personal":{"protocol":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},"models":{"opus":"old-opus"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", original)
	picks := map[string]string{models.Opus: "new-opus", models.Sonnet: "new-sonnet", models.Haiku: "new-haiku"}
	if err := WriteGithubCopilotModels(picks, ""); err != nil {
		t.Fatalf("WriteGithubCopilotModels: %v", err)
	}
	if err := vault.View(func(tx *vault.Tx) error {
		got, ok := tx.Get(vault.Overlay, "providers.prev")
		if !ok {
			t.Error("no previous overlay retained in vault overlay/providers.prev")
		}
		if got != original {
			t.Errorf("previous overlay = %q, want the original overlay %q", got, original)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// WriteGithubCopilotModels (the "GitHub (opencode fallback)" section's pick) changes only the
// github-copilot provider overlay; it must never touch the COPILOT_MODEL pin that copilot's own
// launch and configure page read, even when a pin is already set to something else.
func TestWriteGithubCopilotModelsLeavesCopilotModelPinUntouched(t *testing.T) {
	githubScratch(t)
	t.Setenv(providers.PinCopilotModel, "")
	if err := providers.SetPins(map[string]string{providers.PinCopilotModel: "claude-opus-5.5"}); err != nil {
		t.Fatal(err)
	}
	picks := map[string]string{models.Opus: "gpt-6.1-sol", models.Sonnet: "claude-sonnet-5.5", models.Haiku: "claude-haiku-4.5"}
	if err := WriteGithubCopilotModels(picks, ""); err != nil {
		t.Fatalf("WriteGithubCopilotModels: %v", err)
	}
	if got := providers.Pin(providers.PinCopilotModel); got != "claude-opus-5.5" {
		t.Errorf("COPILOT_MODEL pin = %q, want unchanged claude-opus-5.5: the fallback section must not touch it", got)
	}
}

// fallback="auto" is rejected outright: opencode's own catalog has no "auto" alias, so substituting
// it right back in would defeat the whole point of opencode_fallback. The overlay must stay untouched.
func TestWriteGithubCopilotModelsRejectsAutoFallback(t *testing.T) {
	githubScratch(t)
	picks := map[string]string{models.Opus: "gpt-6.1-sol", models.Sonnet: "claude-sonnet-5.5", models.Haiku: "auto"}
	err := WriteGithubCopilotModels(picks, "auto")
	if err == nil {
		t.Fatal("WriteGithubCopilotModels: want an error rejecting fallback=\"auto\"")
	}
	if !strings.Contains(err.Error(), "auto") {
		t.Errorf("error = %v, want it to name \"auto\"", err)
	}
	if _, err := providers.OverlayDoc(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a rejected fallback must not write the overlay at all: got %v, want fs.ErrNotExist", err)
	}
}

// fallback="" (no slot resolved to "auto") leaves any opencode_fallback the overlay already set alone.
func TestWriteGithubCopilotModelsEmptyFallbackLeavesExistingUnchanged(t *testing.T) {
	githubScratch(t)
	original := `{"providers":{"github":{"personal":{"protocol":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},"opencode_fallback":"mai-code-1.1-flash","models":{"opus":"old-opus"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", original)
	picks := map[string]string{models.Opus: "claude-opus-5.5", models.Sonnet: "claude-sonnet-5.5", models.Haiku: "claude-haiku-4.5"}
	if err := WriteGithubCopilotModels(picks, ""); err != nil {
		t.Fatalf("WriteGithubCopilotModels: %v", err)
	}
	doc := readOverlay(t)
	provs := flatOverlayRows(t, doc["providers"])
	var entry struct {
		OpencodeFallback string `json:"opencode_fallback"`
	}
	if err := json.Unmarshal(provs["personal"], &entry); err != nil {
		t.Fatal(err)
	}
	if entry.OpencodeFallback != "mai-code-1.1-flash" {
		t.Errorf("opencode_fallback = %q, want the pre-existing value left untouched", entry.OpencodeFallback)
	}
}

// Every opus/sonnet/haiku slot must be a non-empty id; a missing one is rejected before anything is written.
func TestWriteGithubCopilotModelsRejectsEmptySlot(t *testing.T) {
	githubScratch(t)
	picks := map[string]string{models.Opus: "gpt-6.1-sol", models.Sonnet: "", models.Haiku: "claude-haiku-4.5"}
	if err := WriteGithubCopilotModels(picks, ""); err == nil {
		t.Fatal("WriteGithubCopilotModels: want an error for an empty sonnet slot")
	}
	if _, err := providers.OverlayDoc(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a rejected pick set must not write the overlay at all: got %v, want fs.ErrNotExist", err)
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
	for _, want := range []string{"gh CLI", "copilot CLI", "route("} {
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
