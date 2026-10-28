package providers

import (
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const ghRow = `"kind":"openai","base_url":"https://api.githubcopilot.com","auth":{"type":"cli","login":"copilot"},` +
	`"models":{"opus":"gpt-6.1-sol","sonnet":"claude-sonnet-5.5","haiku":"auto"}`

// copilotLoginReg is a hermetic vault with github profiles personal (login=edotau) and bare (no login).
func copilotLoginReg(t *testing.T, pin string) *Registry {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{`+
		`"personal":{`+ghRow+`,"login":"edotau"},"bare":{`+ghRow+`}}}}`)
	if pin != "" {
		if err := SetPins(map[string]string{PinCopilotProfile: pin}); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestCopilotLogin(t *testing.T) {
	for _, tc := range []struct{ name, pin, login, profile string }{
		{"profile pin resolves to its login", "personal", "edotau", "personal"},
		{"unset pin is gh's active account", "", "", ""},
		{"a pin naming no github profile is gh's active account", "octocat", "", ""},
		{"profile without a login passes none", "bare", "", "bare"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			login, profile := CopilotLogin(copilotLoginReg(t, tc.pin))
			if login != tc.login || profile != tc.profile {
				t.Errorf("CopilotLogin = (%q, %q), want (%q, %q)", login, profile, tc.login, tc.profile)
			}
		})
	}
}

func TestGithubAccountNameAndLoginRoundTrip(t *testing.T) {
	reg := copilotLoginReg(t, "")
	for _, tc := range []struct{ in, name string }{
		{"edotau", "personal"}, // login -> profile
		{"octocat", "octocat"}, // unmigrated login stays raw
		{"", ""},
	} {
		if got := GithubAccountName(reg, tc.in); got != tc.name {
			t.Errorf("GithubAccountName(%q) = %q, want %q", tc.in, got, tc.name)
		}
	}
	for _, tc := range []struct{ in, login string }{
		{"personal", "edotau"}, {"bare", ""}, {"octocat", "octocat"},
	} {
		if got := GithubAccountLogin(reg, tc.in); got != tc.login {
			t.Errorf("GithubAccountLogin(%q) = %q, want %q", tc.in, got, tc.login)
		}
	}
	if got := GithubAccountLogin(reg, GithubAccountName(reg, "edotau")); got != "edotau" {
		t.Errorf("round trip = %q, want edotau", got)
	}
	if GithubAccountName(nil, "edotau") != "edotau" || GithubAccountLogin(nil, "x") != "x" {
		t.Error("nil registry must pass the value through")
	}
}
