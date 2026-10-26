package databricks

import (
	"encoding/base64"
	"os"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
)

const dbxHost = `"host":"https://adb-1.azuredatabricks.net"`

// seedOverlay isolates the vault in a temp config dir and writes doc as the providers overlay ("" = none).
func seedOverlay(t *testing.T, doc string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if doc == "" {
		return
	}
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "providers", doc) }); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
}

// presetOnly defines databricks-dev as a preset (which injects the beta opt-out into its Env); no -oai twin.
const presetOnly = `{"providers":{"databricks-dev":{"preset":"databricks",` + dbxHost + `}}}`

// twins defines databricks-dev plus a databricks-dev-oai twin; only the base row carries the beta key.
const twins = `{"providers":{"databricks-dev":{"preset":"databricks",` + dbxHost + `},
"databricks-dev-oai":{"kind":"openai","base_url":"https://adb-1.azuredatabricks.net/serving-endpoints",
"auth":{"type":"none"}}}}`

func TestPlanProviderPickByRegistryShape(t *testing.T) {
	cases := []struct {
		name, doc, model, want string
	}{
		{"twins non-claude takes -oai", twins, "databricks-kimi-k3", "databricks-dev-oai"},
		{"twins claude keeps base", twins, "sonnet", "databricks-dev"},
		{"preset-only non-claude keeps base", presetOnly, "databricks-kimi-k3", "databricks-dev"},
		{"preset-only claude keeps base", presetOnly, "sonnet", "databricks-dev"},
		{"no overlay non-claude, no row anywhere: base", "", "databricks-kimi-k3", "databricks-dev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seedOverlay(t, c.doc)
			p, err := plan([]string{c.model, "--env", "dev"})
			if err != nil {
				t.Fatal(err)
			}
			if p.Provider != c.want {
				t.Errorf("provider = %q, want %q", p.Provider, c.want)
			}
		})
	}
}

func TestPlanNoBetaOnlyWhenProviderEnvHasKey(t *testing.T) {
	seedOverlay(t, presetOnly)
	p, _ := plan([]string{"sonnet", "--env", "dev"})
	if !p.NoBeta {
		t.Error("preset provider Env carries the beta key: NoBeta should be true")
	}
	seedOverlay(t, twins)
	p, _ = plan([]string{"databricks-kimi-k3", "--env", "dev"})
	if p.Provider != "databricks-dev-oai" || p.NoBeta {
		t.Errorf("-oai twin has no beta key: got provider %q NoBeta %v", p.Provider, p.NoBeta)
	}
	seedOverlay(t, "")
	p, _ = plan([]string{"sonnet", "--env", "dev"})
	if p.NoBeta {
		t.Error("unknown provider: NoBeta should be false")
	}
}

func TestChildEnvBeta(t *testing.T) {
	t.Setenv(betaKey, "")
	os.Unsetenv(betaKey)
	if slices.Contains(childEnv(Plan{NoBeta: true}), betaOff) {
		t.Error("NoBeta plan must not inject the opt-out")
	}
	if !slices.Contains(childEnv(Plan{}), betaOff) {
		t.Error("plan without NoBeta must inject the opt-out")
	}
}

func TestLaunchBetaInjectionFollowsProvider(t *testing.T) {
	t.Setenv(betaKey, "")
	os.Unsetenv(betaKey)
	seedOverlay(t, presetOnly)
	f := &fakeChild{}
	useFake(t, f)
	if code := Run([]string{"sonnet", "--env", "dev"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if slices.Contains(f.calls[0].env, betaOff) {
		t.Error("preset provider owns the opt-out: launch must not inject it")
	}
	seedOverlay(t, twins)
	f.calls = nil
	if code := Run([]string{"databricks-kimi-k3", "--env", "dev"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !slices.Contains(f.calls[0].env, betaOff) {
		t.Error("-oai twin lacks the key: launch must inject the opt-out")
	}
}

func TestAgentsArgv(t *testing.T) {
	self, _ := os.Executable()
	cases := []struct {
		name        string
		args        []string
		harnessProv string
		wantArgv    []string
		wantCode    int
		wantNoSpawn bool
	}{
		{"on default qa", []string{"agents", "on"}, "", []string{"profile", "use", "--account", "qa", "DEFAULT"}, 0, false},
		{"on --env prod", []string{"agents", "on", "--env", "prod"}, "", []string{"profile", "use", "--account", "prod", "DEFAULT"}, 0, false},
		{"on --env= form", []string{"agents", "on", "--env=dev"}, "", []string{"profile", "use", "--account", "dev", "DEFAULT"}, 0, false},
		{"on infers from HARNESS_PROVIDER", []string{"agents", "on"}, "databricks-prod", []string{"profile", "use", "--account", "prod", "DEFAULT"}, 0, false},
		{"on infers from -oai provider", []string{"agents", "on"}, "databricks-dev-oai", []string{"profile", "use", "--account", "dev", "DEFAULT"}, 0, false},
		{"--env beats HARNESS_PROVIDER", []string{"agents", "on", "--env", "dev"}, "databricks-prod", []string{"profile", "use", "--account", "dev", "DEFAULT"}, 0, false},
		{"unrelated HARNESS_PROVIDER falls to qa", []string{"agents", "on"}, "openai", []string{"profile", "use", "--account", "qa", "DEFAULT"}, 0, false},
		{"off", []string{"agents", "off"}, "", []string{"profile", "off"}, 0, false},
		{"status", []string{"agents", "status"}, "", []string{"profile", "show"}, 0, false},
		{"bad env", []string{"agents", "on", "--env", "staging"}, "", nil, 2, true},
		{"unknown sub", []string{"agents", "bogus"}, "", nil, 2, true},
		{"stray arg", []string{"agents", "on", "extra"}, "", nil, 2, true},
		{"bare agents is help", []string{"agents"}, "", nil, 0, true},
		{"agents -h is help", []string{"agents", "-h"}, "", nil, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seedOverlay(t, "") // the real vault may still hold a legacy databricks-<env> set
			t.Setenv("HARNESS_PROVIDER", c.harnessProv)
			f := &fakeChild{}
			useFake(t, f)
			code, _, _ := runCaptured(t, func() int { return Run(c.args) })
			if code != c.wantCode {
				t.Fatalf("exit = %d, want %d", code, c.wantCode)
			}
			if c.wantNoSpawn {
				if len(f.calls) != 0 {
					t.Fatalf("spawned %d calls, want none", len(f.calls))
				}
				return
			}
			if len(f.calls) != 1 || f.calls[0].bin != self || !slices.Equal(f.calls[0].argv, c.wantArgv) {
				var got [][]string // argv only: a call's env holds the session's variables
				for _, c := range f.calls {
					got = append(got, append([]string{c.bin}, c.argv...))
				}
				t.Fatalf("calls = %q, want one %v on %s", got, c.wantArgv, self)
			}
		})
	}
}
