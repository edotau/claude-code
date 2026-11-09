package copilot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	copilotsdk "github.com/github/copilot-sdk/go"
)

func regressionWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func catalogScratch(t *testing.T) {
	t.Helper()
	pinsScratch(t)
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
	listModelsFn = func(context.Context, SDKOptions) ([]copilotsdk.ModelInfo, error) {
		calls++
		return []copilotsdk.ModelInfo{{ID: "account-b-model"}}, nil
	}
	got, err := RefreshCatalog(context.Background())
	if err == nil || !strings.Contains(err.Error(), "account-a") || len(got) != 0 || calls != 0 {
		t.Fatalf("RefreshCatalog = %v, %v; listings = %d; want refusal before listing account-b", got, err, calls)
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
	listModelsFn = func(_ context.Context, opts SDKOptions) ([]copilotsdk.ModelInfo, error) {
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

func TestQuotaHandoffWriteFailureKeepsPartialAnswer(t *testing.T) {
	pinsScratch(t)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	regressionWrite(t, blocked, "blocked")
	old := handoffDir
	handoffDir = func(string) string { return filepath.Join(blocked, "handoffs") }
	t.Cleanup(func() { handoffDir = old })
	cause := errors.New("402 payment required")
	res, err := CopilotQuotaHandoff(agent.RunOptions{WorkDir: t.TempDir()}, wire.Result{}, "partial-session", "partial answer", cause, nil)
	if res.Answer != "partial answer" || res.SessionID != "partial-session" || res.Handoff != "" {
		t.Fatalf("result = %+v; want partial answer and session without handoff", res)
	}
	if !errors.Is(err, cause) || !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "handoff not written") {
		t.Fatalf("err = %v; want quota and handoff write errors", err)
	}
}

func TestCheckNoGitHubLoginAllowsSpacedEmptyCollections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, body := range []string{`{"loggedInUsers":[ ]}`, `{"copilotTokens":{ }}`} {
		regressionWrite(t, path, body)
		if err := checkNoGitHubLogin(path); err != nil {
			t.Errorf("empty login collection %s refused: %v", body, err)
		}
	}
}

func TestRefreshLinkPreservesDestinationOnStatFailure(t *testing.T) {
	for _, name := range []string{"hooks", "permissions-config.json"} {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "loop")
			if err := os.Symlink(src, src); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), name)
			if err := os.Symlink(src, dst); err != nil {
				t.Fatal(err)
			}
			if err := refreshLink(src, dst); !errors.Is(err, syscall.ELOOP) {
				t.Errorf("refreshLink err = %v; want source stat ELOOP", err)
			}
			if target, err := os.Readlink(dst); err != nil || target != src {
				t.Fatalf("destination = %q, %v; want original symlink to %s", target, err, src)
			}
		})
	}
}

func TestSDKRegistryWarnsAndDropsUnreadableSettings(t *testing.T) {
	pinsScratch(t)
	setGOOS(t, "linux")
	t.Setenv("COPILOT_HOME", t.TempDir())
	if err := os.Mkdir(filepath.Join(os.Getenv("COPILOT_HOME"), "settings.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(copilotRegistryHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(copilotRegistryHome(), "settings.json")
	regressionWrite(t, stale, `{"defaultPermissionMode":"allow-all"}`)
	stubSettingsRegistry(t)
	logPath := filepath.Join(t.TempDir(), "dispatch.log")
	sink := wire.NewLogSink(logPath)
	t.Cleanup(sink.Close)
	ctx := wire.WithLog(context.Background(), sink)
	route, err := prepareCopilotRegistry(ctx, providers.Target{Provider: &providers.Provider{Name: "fixture"}, Model: "claude-sonnet-5-5"})
	if err != nil || route == nil {
		t.Fatalf("prepareCopilotRegistry = %v, %v", route, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale private settings still usable: %v", err)
	}
	sink.Close()
	raw, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(raw), "note: copilot: settings.json not carried over") {
		t.Fatalf("SDK warning log = %q, %v; want settings read warning", raw, err)
	}
}

func stubSettingsRegistry(t *testing.T) {
	t.Helper()
	oldDiscover, oldRouter, oldMint := discoverModels, agent.EnsureRouter, mintSession
	t.Cleanup(func() { discoverModels, agent.EnsureRouter, mintSession = oldDiscover, oldRouter, oldMint })
	discoverModels = func(context.Context, *providers.Registry, *providers.Provider) ([]CopilotEndpoint, error) {
		return []CopilotEndpoint{{ID: "claude-sonnet-5-5"}}, nil
	}
	agent.EnsureRouter = func(context.Context) (string, error) { return "http://127.0.0.1:1", nil }
	mintSession = func(context.Context, string, int) (string, error) { return "fixture-session", nil }
}
