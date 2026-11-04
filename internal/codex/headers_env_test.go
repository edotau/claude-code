package codex

import (
	"context"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// A credential-shaped header value must reach codex through env_http_headers + env, never argv.
func TestCodexCredentialHeaderTravelsInEnvNotArgv(t *testing.T) {
	secret := "sk-" + strings.Repeat("a1B2", 10)
	p := &providers.Provider{Name: "acme", Headers: map[string]string{
		"Authorization": "Bearer " + secret,
		"X-Team":        "platform",
	}}
	env, argv, err := codex(context.Background(), p, "https://example.test/v1", "gpt-x", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, "\n")
	if strings.Contains(joined, secret) {
		t.Fatalf("argv leaks the secret: %s", joined)
	}
	for _, want := range []string{
		`env_http_headers = { "Authorization" = "CLAUDE_CODE_CODEX_HDR_AUTHORIZATION" }`,
		`http_headers = { "X-Team" = "platform" }`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q in %s", want, joined)
		}
	}
	wantEnv := "CLAUDE_CODE_CODEX_HDR_AUTHORIZATION=Bearer " + secret
	var found bool
	for _, e := range env {
		found = found || e == wantEnv
	}
	if !found {
		t.Errorf("env lacks %q: %q", "CLAUDE_CODE_CODEX_HDR_AUTHORIZATION", env)
	}
}
