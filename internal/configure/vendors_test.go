package configure

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// An agy that ignores SIGINT and SIGTERM is SIGKILLed after the grace, so stopAgy returns instead of hanging.
func TestStopAgyEscalatesToKill(t *testing.T) {
	orig := AgyStopGrace
	AgyStopGrace = 100 * time.Millisecond
	t.Cleanup(func() { AgyStopGrace = orig })
	cmd := exec.Command("sh", "-c", `trap "" INT TERM; echo ready; while :; do sleep 0.05; done`)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Read(make([]byte, 6)); err != nil { // "ready\n": the traps are installed
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stopped := make(chan struct{})
	go func() { stopAgy(cmd, done); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("stopAgy did not return for an agy that ignores SIGINT and SIGTERM")
	}
}

func TestStopAgyKillsStubbornDescendant(t *testing.T) {
	if os.Getenv("CONFIGURE_DESCENDANT_HELPER") == "1" {
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
		fmt.Fprintln(os.Stdout, os.Getpid())
		for {
			time.Sleep(time.Hour)
		}
	}
	orig := AgyStopGrace
	AgyStopGrace = 100 * time.Millisecond
	t.Cleanup(func() { AgyStopGrace = orig })
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	cmd := exec.Command("sh", "-c", `trap 'exit 0' INT TERM; "$CONFIGURE_TEST_BINARY" -test.run=^TestStopAgyKillsStubbornDescendant$ & wait`)
	cmd.Env = append(os.Environ(), "CONFIGURE_DESCENDANT_HELPER=1", "CONFIGURE_TEST_BINARY="+bin)
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	_ = w.Close()
	var pid int
	if _, err := fmt.Fscanln(r, &pid); err != nil {
		t.Fatal(err)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Kill() })
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	exited := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, r); close(exited) }()
	stopAgy(cmd, done)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("descendant still holds stdout open after its parent exited and stopAgy returned")
	}
}

func TestHasAdminCredentialRequiresCompleteCredential(t *testing.T) {
	for _, tc := range []struct {
		name, id, secret, key string
		present               bool
	}{
		{"none", "", "", "", false},
		{"id only", "client-id", "", "", false},
		{"secret only", "", "client-secret", "", false},
		{"oauth pair", "client-id", "client-secret", "", true},
		{"api key", "", "", "api-key", true},
		{"api key with incomplete oauth", "client-id", "", "api-key", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.Use(t)
			for name, value := range map[string]string{
				"TAILSCALE_OAUTH_CLIENT_ID":     tc.id,
				"TAILSCALE_OAUTH_CLIENT_SECRET": tc.secret,
				"TAILSCALE_API_KEY":             tc.key,
			} {
				t.Setenv(name, "")
				if value != "" {
					vaulttest.Secret(t, name, value)
				}
			}
			if got := HasAdminCredential(); got != tc.present {
				t.Fatalf("HasAdminCredential = %v, want %v", got, tc.present)
			}
		})
	}
}

// Without a resolvable home the login marker would be a cwd-relative path: refuse before launching agy.

func TestAgyCommandMissingVendorStartFails(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("PATH", t.TempDir())
	cmd := AgyCommand()
	if cmd == nil {
		t.Fatal("agyCommand returned nil")
	}
	if err := cmd.Start(); err == nil {
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
		t.Fatal("Start succeeded without bin/gemini")
	} else if !strings.Contains(err.Error(), filepath.Join(cfg, "bin", "gemini")) ||
		!strings.Contains(err.Error(), "not installed") {
		t.Errorf("Start error = %v, want missing bin/gemini error", err)
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
		maps.Copy(flat, rows)
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
	vaulttest.SetRecord(t, "copilot", "model", "claude-opus-5.5")
	picks := map[string]string{models.Opus: "gpt-6.1-sol", models.Sonnet: "claude-sonnet-5.5", models.Haiku: "claude-haiku-4.5"}
	if err := WriteGithubCopilotModels(picks, ""); err != nil {
		t.Fatalf("WriteGithubCopilotModels: %v", err)
	}
	if got := vault.GetAgentRecord("copilot").Model; got != "claude-opus-5.5" {
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

// fallback="" (no alias resolved to "auto") leaves any opencode_fallback the overlay already set alone.
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

// Every opus/sonnet/haiku alias must be a non-empty id; a missing one is rejected before anything is written.
func TestWriteGithubCopilotModelsRejectsEmptySlot(t *testing.T) {
	githubScratch(t)
	picks := map[string]string{models.Opus: "gpt-6.1-sol", models.Sonnet: "", models.Haiku: "claude-haiku-4.5"}
	if err := WriteGithubCopilotModels(picks, ""); err == nil {
		t.Fatal("WriteGithubCopilotModels: want an error for an empty sonnet alias")
	}
	if _, err := providers.OverlayDoc(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a rejected pick set must not write the overlay at all: got %v, want fs.ErrNotExist", err)
	}
}

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

func TestHasAdminCredential(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TAILSCALE_OAUTH_CLIENT_ID", "")
	t.Setenv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	t.Setenv("TAILSCALE_API_KEY", "")
	t.Setenv("PATH", t.TempDir()) // no secret-tool: the host keyring must not answer
	if HasAdminCredential() {
		t.Error("hasAdminCredential() = true with nothing set, want false")
	}
	t.Setenv("TAILSCALE_API_KEY", "tskey-api-abc")
	if !HasAdminCredential() {
		t.Error("hasAdminCredential() = false with TAILSCALE_API_KEY set, want true")
	}
}
