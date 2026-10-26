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

// m2m service-principal rows are cold-start helpers for minting u2m, so the table hides them and counts them.
func TestProviderTableHidesM2MRows(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reg := &providers.Registry{Default: "dev", Providers: map[string]*providers.Provider{
		"dev": {Name: "dev", Kind: providers.DialectAnthropic, Preset: providers.PresetDatabricks},
		"sp-user-dev": {Name: "sp-user-dev", Kind: providers.DialectAnthropic, Preset: providers.PresetDatabricks,
			Auth: providers.Auth{AuthType: providers.AuthOAuthM2M}},
	}}
	_, out, _ := captureOutput(t, func() int { printProviderTable(reg); return 0 })
	if strings.Contains(out, "sp-user-dev") {
		t.Fatalf("m2m row listed:\n%s", out)
	}
	if !strings.Contains(out, "* dev") || !strings.Contains(out, "hidden: 1 m2m service-principal row(s)") {
		t.Fatalf("want the dev row and a hidden count, got:\n%s", out)
	}
}
