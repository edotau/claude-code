package copilot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	copilotsdk "github.com/github/copilot-sdk/go"
)

func catalogScratch(t *testing.T) {
	t.Helper()
	hosttest.PinScratch(t)
	vaulttest.UseKeyring(t)
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("COPILOT_HOME", t.TempDir())
	for _, key := range copilotTokenEnvs {
		t.Setenv(key, "")
	}
	pathstest.FakeVendor(t, "copilot", "exit 0")
}

func TestRefreshCatalogRefusesMissingPinnedToken(t *testing.T) {
	catalogScratch(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"account-a":{"protocol":"openai","base_url":"https://api.githubcopilot.com","login":"account-a","auth":{"type":"cli","login":"copilot"}}}}}`)
	vaulttest.SetRecord(t, "copilot", "profile", "account-a")
	regressionWrite(t, filepath.Join(os.Getenv("GH_CONFIG_DIR"), "hosts.yml"), "github.com:\n    users:\n        account-a:\n            oauth_token: ignored\n    user: account-a\n")
	regressionWrite(t, filepath.Join(os.Getenv("COPILOT_HOME"), "config.json"), `{"lastLoggedInUser":{"login":"account-b"}}`)
	if auth, ok := copilotAuthSource(); !ok || auth.Account != "account-a" {
		t.Fatalf("auth = %+v, %v; want account-a pinned", auth, ok)
	}
	old := listModelsFn
	t.Cleanup(func() { listModelsFn = old })
	calls := 0
	listModelsFn = func(context.Context, sdkOptions) ([]copilotsdk.ModelInfo, error) {
		calls++
		return []copilotsdk.ModelInfo{{ID: "account-b-model"}}, nil
	}
	got, err := refreshCatalog(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "account-a") || len(got) != 0 || calls != 0 {
		t.Fatalf("refreshCatalog = %v, %v; listings = %d; want refusal before listing account-b", got, err, calls)
	}
	if _, found, err := vault.ReadSecret(catalogRecord); err != nil || found {
		t.Fatalf("snapshot found = %v, err = %v; want no mislabeled snapshot", found, err)
	}
}

func TestCatalogRefreshesAfterEnvironmentTokenChange(t *testing.T) {
	catalogScratch(t)
	t.Setenv(providers.CopilotTokenEnv, "fixture-token-a")
	old := listModelsFn
	t.Cleanup(func() { listModelsFn = old })
	calls := 0
	listModelsFn = func(_ context.Context, opts sdkOptions) ([]copilotsdk.ModelInfo, error) {
		calls++
		return []copilotsdk.ModelInfo{{ID: opts.GitHubToken + "-model"}}, nil
	}
	assertCatalogModel(t, "fixture-token-a-model")
	assertCatalogModel(t, "fixture-token-a-model")
	if calls != 1 {
		t.Fatalf("unchanged token made %d listings; want one cached listing", calls)
	}
	t.Setenv(providers.CopilotTokenEnv, "fixture-token-b")
	assertCatalogModel(t, "fixture-token-b-model")
	assertCatalogModel(t, "fixture-token-b-model")
	if calls != 2 {
		t.Fatalf("changed token made %d listings; want one per token", calls)
	}
}

func assertCatalogModel(t *testing.T, want string) {
	t.Helper()
	got, err := Catalog(context.Background())
	if err != nil || len(got) != 1 || got[0].ID != want {
		t.Fatalf("Catalog = %v, %v; want %s", got, err, want)
	}
}

// TestDiscoverPickerProfilesSkipsUnusableRows: a keyless API row and an empty listing are skips, never errors.
func TestDiscoverPickerProfilesSkipsUnusableRows(t *testing.T) {
	catalogScratch(t)
	orig := discoverLive
	t.Cleanup(func() { discoverLive = orig })
	listings := map[string][]providers.ModelInfo{"listed": {{ID: "live-model-a"}, {ID: "live-model-b"}}, "empty": nil}
	discoverLive = func(_ context.Context, p *providers.Provider) ([]providers.ModelInfo, error) {
		live, ok := listings[p.Name]
		if !ok {
			return nil, errors.New("catalog request failed (HTTP 503) for " + p.Name)
		}
		return live, nil
	}
	gh := func(name string) *providers.Provider {
		return &providers.Provider{Name: name, Provider: "github", Login: "someone", Auth: providers.Auth{Type: providers.AuthCLI}}
	}
	keyless := &providers.Provider{Name: "keyless", Provider: "anthropic", Protocol: providers.ProtocolAnthropic,
		BaseURL: "https://api.anthropic.com", Auth: providers.Auth{Type: providers.AuthKey, Env: "ANTHROPIC_API_KEY"}}
	for _, tc := range []struct {
		p              *providers.Provider
		status, note   string
		wantDiscovered int
	}{
		{keyless, "skipped", "vault set ANTHROPIC_KEYLESS_KEY", 0}, // never reaches the stub: "keyless" would be a 503 error
		{gh("empty"), "skipped", "no models", 0},
		{gh("listed"), "preview", "", 2},
		{gh("down"), "error", "HTTP 503", 0},
	} {
		r := discoverPickerProfile(tc.p, false)
		if r.Status != tc.status || !strings.Contains(r.Note, tc.note) || r.Discovered != tc.wantDiscovered {
			t.Errorf("%s = %q discovered=%d note=%q, want %q discovered=%d note containing %q",
				tc.p.Name, r.Status, r.Discovered, r.Note, tc.status, tc.wantDiscovered, tc.note)
		}
	}
}
