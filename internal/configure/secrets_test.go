package configure

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
)

func TestParseAssignment(t *testing.T) {
	cases := []struct {
		raw, name, value string
	}{
		{"sk-bare-value", "", "sk-bare-value"},
		{"NAME=v", "NAME", "v"},
		{`export NAME="v"`, "NAME", "v"},
		{"NAME='v'", "NAME", "v"},
		{"abc==", "", "abc=="},   // base64 padding stays a bare value, never split as NAME=value
		{"ABC=", "", "ABC="},     // trailing '=' with no value: Cut's v=="" rule keeps it bare
		{"name=v", "", "name=v"}, // lowercase "key" is not an env-var-shaped name: stays bare
	}
	for _, c := range cases {
		name, value := ParseAssignment(c.raw)
		if name != c.name || value != c.value {
			t.Errorf("ParseAssignment(%q) = (%q, %q), want (%q, %q)", c.raw, name, value, c.name, c.value)
		}
	}
}

// Picking "+ new" on the non-TTY path, entering a name and value, stores to the file plane (no
// keyring on PATH) and the transcript shows the redacted form, never the raw value.
func TestSecretsAddCustomStoresFileAndRedacts(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("PATH", t.TempDir()) // no keyring tool: forces the file plane
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	newRow := len(trackedSecrets(reg)) + 1 // "+ new" is the row right after every tracked secret
	in := strings.NewReader(fmt.Sprintf("%d\nMY_NEW_SECRET\nsk-super-secret-raw-value\n\n", newRow))
	var out bytes.Buffer
	rc := Secrets(Options{In: in, Out: &out}, false)
	if rc != 0 {
		t.Fatalf("Secrets rc = %d:\n%s", rc, out.String())
	}
	transcript := out.String()
	if strings.Contains(transcript, "sk-super-secret-raw-value") {
		t.Fatalf("transcript leaked the raw value:\n%s", transcript)
	}
	if !strings.Contains(transcript, "MY_NEW_SECRET") {
		t.Errorf("transcript does not name MY_NEW_SECRET:\n%s", transcript)
	}
}

func TestSourceLabel(t *testing.T) {
	cases := map[string]string{"": "missing", "file": "file:plaintext", "keyring": "keyring", "env": "env"}
	for src, want := range cases {
		if got := sourceLabel(src); got != want {
			t.Errorf("sourceLabel(%q) = %q, want %q", src, got, want)
		}
	}
}

// The list is harness-read names plus the user's own: a console-URL key no enabled provider reads is not a row,
// an indexed custom secret is.
func TestTrackedSecretsListsHarnessAndCustomOnly(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("PATH", t.TempDir())
	if err := os.WriteFile(filepath.Join(mkEnvD(t, cfg), "providers.json"), []byte(`{"providers": {"openrouter": null}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Store("MY_CUSTOM", "v", false); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, s := range trackedSecrets(reg) {
		rows[s.name] = s.users
	}
	if _, ok := rows["OPENROUTER_API_KEY"]; ok {
		t.Error("OPENROUTER_API_KEY listed although its provider is disabled")
	}
	if rows["MY_CUSTOM"] != "custom" || rows[providers.OAuthTokenEnv] == "" {
		t.Errorf("rows = %v, want MY_CUSTOM (custom) and the setup-token", rows)
	}
}
