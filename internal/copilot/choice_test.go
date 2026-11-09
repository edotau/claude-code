package copilot

import (
	"context"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

// An empty target model lets the default apply instead of failing as the unknown model "".
func TestPrepareCopilotRegistryEmptyModelUsesDefault(t *testing.T) {
	pinsScratch(t)
	setGOOS(t, "linux")
	t.Setenv("COPILOT_HOME", t.TempDir())
	stubSettingsRegistry(t)
	route, err := prepareCopilotRegistry(context.Background(), providers.Target{Provider: &providers.Provider{Name: "fixture"}})
	if err != nil || route == nil || route.ref == "" {
		t.Fatalf("prepareCopilotRegistry(empty model) = %+v, %v; want the default model selected", route, err)
	}
}

// With no flag and no pin, the profile's default model is the choice; a flag or pin still wins.
func TestCopilotChoiceProfileDefault(t *testing.T) {
	pinsScratch(t)
	t.Setenv("COPILOT_MODEL", "")
	if got := copilotChoice(nil, "claude-sonnet-5-5").Requested; !slices.Equal(got, []string{"claude-sonnet-5-5"}) {
		t.Errorf("no flag/pin: Requested = %v, want the profile default", got)
	}
	if got := copilotChoice([]string{"gpt-5-5"}, "claude-sonnet-5-5").Requested; !slices.Equal(got, []string{"gpt-5-5"}) {
		t.Errorf("flag: Requested = %v, want the flag only", got)
	}
	t.Setenv("COPILOT_MODEL", "kimi-k3")
	if c := copilotChoice(nil, "claude-sonnet-5-5"); len(c.Requested) != 0 || c.Pin != "kimi-k3" {
		t.Errorf("pin: choice = %+v, want the pin and no profile default", c)
	}
}

// The quota hand-off masks --env values and the registry token, not only the route env.
func TestRunCredentialsCoverEnvAndRegistryToken(t *testing.T) {
	opts := SDKOptions{Token: func() (string, error) { return "sess-token-123456", nil }}
	req := agent.RunOptions{Env: []string{"INTERNAL_SECRET=plain-value-12345", "EMPTY="}}
	got := runCredentials(opts, []string{"DBX_TOKEN=route-secret-1"}, req)
	for _, want := range []string{"route-secret-1", "plain-value-12345", "sess-token-123456"} {
		if !slices.Contains(got, want) {
			t.Errorf("credentials %v miss %q", got, want)
		}
	}
	if slices.Contains(got, "") {
		t.Errorf("credentials %v hold an empty value", got)
	}
}

// A plain --env value (a path) must stay out of the redaction list; secret-named ones are masked.
func TestRunCredentialsIgnoresNonSecretEnv(t *testing.T) {
	req := agent.RunOptions{Env: []string{"WORKDIR=/home/user/long/project/dir", "API_TOKEN=s3cr3t-value-123"}}
	got := runCredentials(SDKOptions{}, nil, req)
	if !slices.Equal(got, []string{"s3cr3t-value-123"}) {
		t.Fatalf("runCredentials = %q; want only the secret-named value", got)
	}
}
