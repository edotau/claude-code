package tailscale

import (
	"context"
	"errors"
	"testing"
)

// hermeticSecrets blocks the keyring and file plane so secrets.Get resolves from the process env only.
func hermeticSecrets(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, n := range []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET", "TAILSCALE_API_KEY"} {
		t.Setenv(n, "")
	}
}

func TestNewClientErrNoCredential(t *testing.T) {
	hermeticSecrets(t)
	if _, err := NewClient(); !errors.Is(err, ErrNoCredential) {
		t.Errorf("NewClient() err = %v, want ErrNoCredential", err)
	}
}

// NewClient's OAuth-vs-API-key preference is not exercised here: calling the returned token func
// for the OAuth branch would POST to the real DefaultBaseURL, which tests must never do.

func TestNewClientFallsBackToAPIKey(t *testing.T) {
	hermeticSecrets(t)
	t.Setenv("TAILSCALE_API_KEY", "tskey-api-abc")
	c, err := NewClient()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.token(context.Background())
	if err != nil || tok != "tskey-api-abc" {
		t.Errorf("token = (%q, %v), want (tskey-api-abc, nil)", tok, err)
	}
}
