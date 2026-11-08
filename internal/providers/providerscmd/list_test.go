package providerscmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/describe"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// providerListCmdJSON runs `configure providers list <name> --format json`, requires exit 0, and returns stdout and stderr.
func providerListCmdJSON(t *testing.T, name string) (stdout, stderr string) {
	t.Helper()
	code, stdout, stderr := captureOutput(t, func() int { return ProviderList(name, "json", func() []describe.Verb { return nil }) })
	if code != 0 {
		t.Fatalf("provider list %q JSON exit %d stderr %q", name, code, stderr)
	}
	return stdout, stderr
}

func TestProviderListJSONCarriesHostNoSecret(t *testing.T) {
	useVaultTest(t)
	const secret = "dapi-list-secret-value-123456"
	err := providers.SetProviderFields("dbx-host", map[string]any{
		"protocol": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "pat", "accountId": "1234abcd-0000", "workspaceId": "42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := vault.Store("DATABRICKS_DBX_HOST_PAT", secret); err != nil {
		t.Fatal(err)
	}
	out, _ := providerListCmdJSON(t, "dbx-host")
	var e struct{ Host, AccountID, WorkspaceID string }
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	e.Host, _ = raw["host"].(string)
	e.AccountID, _ = raw["accountId"].(string)
	e.WorkspaceID, _ = raw["workspaceId"].(string)
	if e.Host != "https://example.cloud.databricks.com" || e.AccountID != "1234abcd-0000" || e.WorkspaceID != "42" {
		t.Errorf("host/ids = %+v\n%s", e, out)
	}
	if strings.Contains(out, secret) {
		t.Error("list output carries the sealed secret")
	}
	all, _ := providerListCmdJSON(t, "")
	if !strings.Contains(all, `"host"`) || strings.Contains(all, secret) {
		t.Error("full list: secret leaked or host missing")
	}
}

func summaryRegistry() *providers.Registry {
	dbx := func(name string, auth providers.Auth) *providers.Provider {
		return &providers.Provider{Name: name, Provider: providers.GroupDatabricks, Protocol: providers.ProtocolAnthropic, Preset: providers.PresetDatabricks,
			BaseURL: "https://dbc-1.cloud.databricks.com/serving-endpoints/anthropic",
			Routes:  map[string]string{providers.ProtocolResponses: "https://dbc-1.cloud.databricks.com/serving-endpoints"},
			Auth:    auth}
	}
	return &providers.Registry{Default: "dev", Providers: map[string]*providers.Provider{
		"prod":    dbx("prod", providers.Auth{Type: providers.AuthKey, Command: "printf t"}),
		"dev":     dbx("dev", providers.Auth{Type: providers.AuthKey, Command: "printf t"}),
		"sp-user": dbx("sp-user", providers.Auth{Type: providers.AuthM2M, Env: "SUMMARY_SP_SECRET"}),
		"openai": {Name: "openai", Provider: "openai", Protocol: providers.ProtocolOpenAI, BaseURL: "https://api.openai.com/v1",
			Auth: providers.Auth{Type: providers.AuthKey, Env: "SUMMARY_MISSING_KEY"}},
	}}
}

// tableRow returns the first line of out whose first field is label.
func tableRow(out, label string) string {
	for l := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == label {
			return l
		}
	}
	return ""
}

// The summary lists profiles whose credential resolves, folds m2m rows into MODE, and adds an agent section.
func TestProviderTableSummary(t *testing.T) {
	useVaultTest(t)
	t.Setenv("SUMMARY_MISSING_KEY", "")
	t.Setenv("SUMMARY_SP_SECRET", "x")
	reg := summaryRegistry()
	_, out, _ := captureOutput(t, func() int { printProviderTable(reg); return 0 })
	if !strings.Contains(out, "PROVIDERS") || !strings.Contains(out, "AUTH") {
		t.Fatalf("no header:\n%s", out)
	}
	if strings.Contains(out, "sp-user") || strings.Contains(out, "openai ") {
		t.Fatalf("m2m row or unconfigured provider listed as a profile:\n%s", out)
	}
	if r := tableRow(out, "databricks"); !strings.Contains(r, "*dev, prod") || !strings.Contains(r, "oauth, m2m") {
		t.Fatalf("databricks row = %q, want profiles *dev, prod and auths oauth, m2m", r)
	}
	if r := tableRow(out, "codex"); !strings.Contains(r, "*dev, prod") {
		t.Fatalf("codex row = %q, want the Databricks profiles it can run on", r)
	}
}

const credSourceValue = "obviously-fake-source-value"

func credSourceName(source string) string { return "fake-" + strings.ReplaceAll(source, ".", "-") }

// credSourceFixture seeds one provider per credential source and returns the sources plus the text provider table.
func credSourceFixture(t *testing.T) (map[string]providers.Auth, string) {
	t.Helper()
	useVaultTest(t)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(providers.EnvSessionProfile, "")
	t.Setenv("SOURCE_ENV_KEY", credSourceValue)
	t.Setenv("SOURCE_VAULT_KEY", "")
	t.Setenv("SOURCE_MISSING_KEY", "")
	if err := vault.Store("SOURCE_VAULT_KEY", credSourceValue); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "fake-token")
	if err := os.WriteFile(file, []byte(credSourceValue), 0o600); err != nil {
		t.Fatal(err)
	}
	auths := map[string]providers.Auth{
		"vault":     {Type: providers.AuthKey, Env: "SOURCE_VAULT_KEY"},
		"env":       {Type: providers.AuthKey, Env: "SOURCE_ENV_KEY"},
		"auth.file": {Type: providers.AuthKey, File: file},
		"command":   {Type: providers.AuthKey, Command: "printf obviously-fake-command"},
		"cli":       {Type: providers.AuthCLI, Login: "fake-login"},
		"missing":   {Type: providers.AuthKey, Env: "SOURCE_MISSING_KEY"},
		"none":      {Type: providers.AuthNone},
	}
	rows := map[string]*providers.Provider{}
	for source, auth := range auths {
		name := credSourceName(source)
		rows[name] = &providers.Provider{
			Name: name, Protocol: providers.ProtocolOpenAI, BaseURL: "https://example.invalid", Auth: auth,
		}
	}
	raw, err := json.Marshal(map[string]any{"default": "fake-none",
		"providers": map[string]any{"openai": rows}})
	if err != nil {
		t.Fatal(err)
	}
	vaulttest.Put(t, vault.Overlay, "providers", string(raw))
	code, text, _ := captureOutput(t, func() int { return ProviderList("", "text", nil) })
	if code != 0 {
		t.Fatalf("provider table exit %d", code)
	}
	return auths, text
}

func assertCredentialSource(t *testing.T, source, text string) {
	t.Helper()
	name := credSourceName(source)
	stdout, stderr := providerListCmdJSON(t, name)
	if strings.Contains(stdout+stderr, credSourceValue) {
		t.Fatal("provider JSON leaked a raw value")
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
	assertCredentialDetail(t, name, source, note, text)
}

func assertCredentialDetail(t *testing.T, name, source, note, text string) {
	t.Helper()
	code, detail, _ := captureOutput(t, func() int { return ProviderList(name, "text", nil) })
	if code != 0 || !strings.Contains(detail, "cred      "+source) ||
		(note != "" && !strings.Contains(detail, note)) {
		t.Error("provider text detail omitted source or note")
	}
	if strings.Contains(text+detail, credSourceValue) {
		t.Fatal("provider text leaked a raw value")
	}
}

func TestProviderCredentialSources(t *testing.T) {
	auths, text := credSourceFixture(t)
	for source := range auths {
		t.Run(source, func(t *testing.T) { assertCredentialSource(t, source, text) })
	}
	stdout, _ := providerListCmdJSON(t, "")
	var list struct {
		List []providerListEntry `json:"list"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatal("provider list JSON failed")
	}
	for _, entry := range list.List {
		if entry.Source == "" {
			t.Errorf("provider %s has no source", entry.Name)
		}
	}
}

// A qualified <provider>·<profile> name resolves like reg.Get and lists that profile, not panics.
func TestProviderListJSONQualifiedName(t *testing.T) {
	useVaultTest(t)
	if err := providers.SetProviderFields("grp-dev", map[string]any{
		"provider": "databricks", "protocol": "anthropic", "preset": "databricks", "host": "https://example.cloud.databricks.com",
		"auth": map[string]any{"type": "pat"},
	}); err != nil {
		t.Fatal(err)
	}
	out, _ := providerListCmdJSON(t, "databricks·grp-dev")
	var raw struct{ Name string }
	if err := json.Unmarshal([]byte(out), &raw); err != nil || raw.Name != "grp-dev" {
		t.Fatalf("got name %q err %v from %s, want grp-dev", raw.Name, err, out)
	}
}
