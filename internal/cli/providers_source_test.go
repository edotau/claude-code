package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestProviderCredentialSources(t *testing.T) {
	useVaultTest(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HARNESS_PROVIDER", "")
	const value = "obviously-fake-source-value"
	t.Setenv("SOURCE_ENV_KEY", value)
	t.Setenv("SOURCE_VAULT_KEY", "")
	t.Setenv("SOURCE_MISSING_KEY", "")
	if err := secrets.Store("SOURCE_VAULT_KEY", value); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "fake-token")
	if err := os.WriteFile(file, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	auths := map[string]providers.Auth{
		"vault":       {Type: providers.AuthBearer, Env: "SOURCE_VAULT_KEY"},
		"env":         {Type: providers.AuthBearer, Env: "SOURCE_ENV_KEY"},
		"auth.file":   {Type: providers.AuthBearer, File: file},
		"command":     {Type: providers.AuthBearer, Command: "printf obviously-fake-command"},
		"passthrough": {Type: providers.AuthPassthrough, Login: "fake-login"},
		"missing":     {Type: providers.AuthBearer, Env: "SOURCE_MISSING_KEY"},
		"none":        {Type: providers.AuthNone},
	}
	reg := &providers.Registry{Default: "fake-none", Providers: map[string]*providers.Provider{}}
	for source, auth := range auths {
		name := "fake-" + strings.ReplaceAll(source, ".", "-")
		reg.Providers[name] = &providers.Provider{
			Name: name, Kind: providers.DialectOpenAI, BaseURL: "https://example.invalid", Auth: auth,
		}
	}
	raw, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Overlay, "providers", string(raw))
	code, text, _ := captureOutput(t, func() int { return configureProviderList("", "text") })
	if code != 0 {
		t.Fatalf("provider table exit %d", code)
	}
	for source := range auths {
		name := "fake-" + strings.ReplaceAll(source, ".", "-")
		t.Run(source, func(t *testing.T) {
			found := false
			for row := range strings.SplitSeq(text, "\n") {
				fields := strings.Fields(strings.TrimPrefix(row, "*"))
				if len(fields) >= 3 && fields[0] == name {
					found = fields[2] == source
				}
			}
			if !found {
				t.Errorf("provider table missing %s source for %s", source, name)
			}
			code, stdout, stderr := captureOutput(t, func() int { return configureProviderList(name, "json") })
			if strings.Contains(stdout+stderr, value) {
				t.Fatal("provider JSON leaked a raw value")
			}
			if code != 0 {
				t.Fatalf("provider detail JSON exit %d", code)
			}
			var entry map[string]json.RawMessage
			if err := json.Unmarshal([]byte(stdout), &entry); err != nil {
				t.Fatal(err)
			}
			var got, note string
			_ = json.Unmarshal(entry["source"], &got)
			_ = json.Unmarshal(entry["note"], &note)
			if got != source || entry["note"] == nil {
				t.Errorf("JSON source=%q, want %q; note present=%v", got, source, entry["note"] != nil)
			}
			if source == "missing" && note != "set SOURCE_MISSING_KEY" {
				t.Errorf("missing credential note=%q", note)
			}
			code, detail, _ := captureOutput(t, func() int { return configureProviderList(name, "text") })
			if code != 0 || !strings.Contains(detail, "cred      "+source) ||
				(note != "" && !strings.Contains(detail, note)) {
				t.Error("provider text detail omitted source or note")
			}
			if strings.Contains(text+detail, value) {
				t.Fatal("provider text leaked a raw value")
			}
		})
	}
	code, stdout, _ := captureOutput(t, func() int { return configureProviderList("", "json") })
	var list struct {
		List []providerListEntry `json:"list"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil || code != 0 {
		t.Fatal("provider list JSON failed")
	}
	for _, entry := range list.List {
		if entry.Source == "" {
			t.Errorf("provider %s has no source", entry.Name)
		}
	}
}
