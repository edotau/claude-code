package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestProviderGroupByPresetAuthAndHost(t *testing.T) {
	row := func(url string) *providers.Provider {
		return &providers.Provider{Kind: providers.DialectOpenAI, BaseURL: url}
	}
	cases := []struct {
		p    *providers.Provider
		want string
	}{
		{&providers.Provider{Preset: providers.PresetDatabricks}, "databricks"},
		{&providers.Provider{Kind: providers.DialectOpenAI, Auth: providers.Auth{AuthType: providers.AuthU2M}}, "databricks"},
		{row("https://dbc-1.cloud.databricks.com/serving-endpoints"), "databricks"},
		{row("https://api.openai.com/v1"), "openai"},
		{row("https://chatgpt.com/backend-api/codex"), "openai"},
		{row("https://api.githubcopilot.com"), "github"},
		{row("https://generativelanguage.googleapis.com/v1beta/openai"), "google"},
		{row("https://integrate.api.nvidia.com/v1"), "opensource"},
		{row("http://127.0.0.1:11434/v1"), "opensource"},
		{row("https://notopenai.com/v1"), "opensource"}, // suffix match needs a dot boundary
	}
	for _, c := range cases {
		if got := providerGroup(c.p); got != c.want {
			t.Errorf("providerGroup(%s %s) = %q, want %q", c.p.BaseURL, c.p.Preset, got, c.want)
		}
	}
}

func TestEnvSortKeyKeepsDevQaProdTogether(t *testing.T) {
	names := []string{"prod", "x-user-prod", "qa", "x-user-dev", "dev", "x-user-qa", "zeta"}
	slices.SortStableFunc(names, func(a, b string) int { return strings.Compare(envSortKey(a), envSortKey(b)) })
	want := []string{"dev", "qa", "prod", "x-user-dev", "x-user-qa", "x-user-prod", "zeta"}
	if !slices.Equal(names, want) {
		t.Fatalf("sorted = %v, want %v", names, want)
	}
}

// The summary lists profiles whose credential resolves, folds m2m rows into MODE, and adds an agent section.
func TestProviderTableSummary(t *testing.T) {
	useVaultTest(t)
	t.Setenv("SUMMARY_MISSING_KEY", "")
	t.Setenv("SUMMARY_SP_SECRET", "x")
	dbx := func(name string, auth providers.Auth) *providers.Provider {
		return &providers.Provider{Name: name, Kind: providers.DialectAnthropic, Preset: providers.PresetDatabricks,
			BaseURL: "https://dbc-1.cloud.databricks.com/serving-endpoints/anthropic",
			Routes:  map[string]string{providers.DialectResponses: "https://dbc-1.cloud.databricks.com/serving-endpoints"},
			Auth:    auth}
	}
	reg := &providers.Registry{Default: "dev", Providers: map[string]*providers.Provider{
		"prod":    dbx("prod", providers.Auth{Type: providers.AuthBearer, Command: "printf t"}),
		"dev":     dbx("dev", providers.Auth{Type: providers.AuthBearer, Command: "printf t"}),
		"sp-user": dbx("sp-user", providers.Auth{Type: providers.AuthBearer, AuthType: providers.AuthOAuthM2M, Env: "SUMMARY_SP_SECRET"}),
		"openai": {Name: "openai", Kind: providers.DialectOpenAI, BaseURL: "https://api.openai.com/v1",
			Auth: providers.Auth{Type: providers.AuthBearer, Env: "SUMMARY_MISSING_KEY"}},
	}}
	_, out, _ := captureOutput(t, func() int { printProviderTable(reg); return 0 })
	if !strings.Contains(out, "PROVIDERS") || !strings.Contains(out, "MODE") {
		t.Fatalf("no header:\n%s", out)
	}
	if strings.Contains(out, "sp-user") || strings.Contains(out, "openai ") {
		t.Fatalf("m2m row or unconfigured provider listed as a profile:\n%s", out)
	}
	row := func(label string) string {
		for l := range strings.SplitSeq(out, "\n") {
			if f := strings.Fields(l); len(f) > 0 && f[0] == label {
				return l
			}
		}
		return ""
	}
	if r := row("databricks"); !strings.Contains(r, "*dev, prod") || !strings.Contains(r, "web, oauth") {
		t.Fatalf("databricks row = %q, want profiles *dev, prod and modes web, oauth", r)
	}
	if r := row("codex"); !strings.Contains(r, "*dev, prod") {
		t.Fatalf("codex row = %q, want the Databricks profiles it can run on", r)
	}
}

func TestProviderModeLabels(t *testing.T) {
	cases := []struct {
		p    *providers.Provider
		want string
	}{
		{&providers.Provider{Auth: providers.Auth{AuthType: providers.AuthU2M}}, "web"},
		{&providers.Provider{Preset: providers.PresetDatabricks}, "web"},
		{&providers.Provider{Auth: providers.Auth{AuthType: providers.AuthOAuthM2M}}, "oauth"},
		{&providers.Provider{Auth: providers.Auth{Type: providers.AuthPassthrough}}, "login"},
		{&providers.Provider{Auth: providers.Auth{Type: providers.AuthNone}}, "-"},
		{&providers.Provider{Auth: providers.Auth{Type: providers.AuthBearer, Env: "K"}}, "api-key"},
		{&providers.Provider{Auth: providers.Auth{AuthType: providers.AuthPAT}}, "api-key"},
	}
	for _, c := range cases {
		if got := providerMode(c.p); got != c.want {
			t.Errorf("providerMode(%+v) = %q, want %q", c.p.Auth, got, c.want)
		}
	}
}
