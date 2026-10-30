package vendors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const ghProfileRow = `"protocol":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},` +
	`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"auto"}`

// ghArgsOnPin runs the launch-token path with COPILOT_PROFILE=pin on a migrated hermetic vault and returns gh's argv.
func ghArgsOnPin(t *testing.T, pin string) string {
	t.Helper()
	copilotAuthEnv(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{`+
		`"personal":{`+ghProfileRow+`,"login":"edotau"},"bare":{`+ghProfileRow+`}}}}`)
	ghDir := os.Getenv("GH_CONFIG_DIR")
	hosts := "github.com:\n    user: alice\n    users:\n        alice:\n            oauth_token: t\n" +
		"        edotau:\n            oauth_token: t\n        octocat:\n            oauth_token: t\n"
	if err := os.WriteFile(filepath.Join(ghDir, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\necho tok\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if pin != "" {
		if err := providers.SetPins(map[string]string{providers.PinCopilotProfile: pin}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := CopilotLaunchToken(); err != nil {
		t.Fatalf("CopilotLaunchToken(pin %q): %v", pin, err)
	}
	raw, _ := os.ReadFile(log)
	return strings.TrimSpace(string(raw))
}

func TestCopilotProfilePinPassesLoginToGh(t *testing.T) {
	for _, tc := range []struct{ name, pin, want string }{
		{"profile pin passes its login", "personal", "auth token -u edotau"},
		{"unset pin passes no user", "", "auth token"},
		{"profile with no login passes no user", "bare", "auth token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ghArgsOnPin(t, tc.pin); got != tc.want {
				t.Errorf("gh argv = %q, want %q", got, tc.want)
			}
		})
	}
}
