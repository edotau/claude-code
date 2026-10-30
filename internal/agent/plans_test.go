package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func writeCodexAuth(t *testing.T, idToken string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	body := `{"tokens":{"id_token":"` + idToken + `"}}`
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func unsignedJWT(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(payload)) + "."
}

func TestCodexPlan(t *testing.T) {
	cases := []struct{ name, token, want string }{
		{"plus", unsignedJWT(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"plus"}}`), "plus"},
		{"pro", unsignedJWT(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"pro"}}`), "pro"},
		{"claim absent", unsignedJWT(`{"sub":"x"}`), ""},
		{"payload not json", unsignedJWT(`not json`), ""},
		{"payload not base64", "a.!!!.c", ""},
		{"not a jwt", "opaque", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			writeCodexAuth(t, c.token)
			if got := CodexPlan(); got != c.want {
				t.Errorf("CodexPlan = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCodexPlanMissingOrMalformedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	// Stub the keyring fallback absent: without this, a real "Codex Auth" keychain item on the
	// machine running the test (e.g. a signed-in developer box) would leak through codexAuthJSON.
	orig := codexKeychainCmd
	codexKeychainCmd = func(ctx context.Context, args ...string) ([]byte, error) { return nil, nil }
	defer func() { codexKeychainCmd = orig }()
	if got := CodexPlan(); got != "" {
		t.Errorf("missing auth.json: CodexPlan = %q", got)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := CodexPlan(); got != "" {
		t.Errorf("malformed auth.json: CodexPlan = %q", got)
	}
}
