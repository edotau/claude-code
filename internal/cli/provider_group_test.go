package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

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
		return &providers.Provider{Name: name, Provider: providers.GroupDatabricks, Kind: providers.DialectAnthropic, Preset: providers.PresetDatabricks,
			BaseURL: "https://dbc-1.cloud.databricks.com/serving-endpoints/anthropic",
			Routes:  map[string]string{providers.DialectResponses: "https://dbc-1.cloud.databricks.com/serving-endpoints"},
			Auth:    auth}
	}
	reg := &providers.Registry{Default: "dev", Providers: map[string]*providers.Provider{
		"prod":    dbx("prod", providers.Auth{Type: providers.AuthKey, Command: "printf t"}),
		"dev":     dbx("dev", providers.Auth{Type: providers.AuthKey, Command: "printf t"}),
		"sp-user": dbx("sp-user", providers.Auth{Type: providers.AuthM2M, Env: "SUMMARY_SP_SECRET"}),
		"openai": {Name: "openai", Provider: "openai", Kind: providers.DialectOpenAI, BaseURL: "https://api.openai.com/v1",
			Auth: providers.Auth{Type: providers.AuthKey, Env: "SUMMARY_MISSING_KEY"}},
	}}
	_, out, _ := captureOutput(t, func() int { printProviderTable(reg); return 0 })
	if !strings.Contains(out, "PROVIDERS") || !strings.Contains(out, "AUTH") {
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
	if r := row("databricks"); !strings.Contains(r, "*dev, prod") || !strings.Contains(r, "oauth, m2m") {
		t.Fatalf("databricks row = %q, want profiles *dev, prod and auths oauth, m2m", r)
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
		{&providers.Provider{Auth: providers.Auth{Type: providers.AuthOAuth}}, "oauth"},
		{&providers.Provider{Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: providers.AuthKey}}, "oauth"},
		{&providers.Provider{Auth: providers.Auth{Type: providers.AuthM2M}}, "m2m"},
		{&providers.Provider{Auth: providers.Auth{Type: providers.AuthCLI}}, "cli"},
		{&providers.Provider{Auth: providers.Auth{Type: providers.AuthNone}}, "-"},
		{&providers.Provider{Auth: providers.Auth{Type: providers.AuthKey, Env: "K"}}, "key"},
		{&providers.Provider{Auth: providers.Auth{Type: providers.AuthPAT}}, "pat"},
	}
	for _, c := range cases {
		if got := providerMode(c.p); got != c.want {
			t.Errorf("providerMode(%+v) = %q, want %q", c.p.Auth, got, c.want)
		}
	}
}
