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

// presetOnly defines one preset profile (which injects the beta opt-out into its Env).
const presetOnly = `{"providers":{"databricks":{"aa":{"preset":"databricks",` + dbxHost + `}}}}`

func TestPlanNoBetaOnlyWhenProviderEnvHasKey(t *testing.T) {
	seedOverlay(t, presetOnly)
	p, err := plan([]string{"sonnet", "--profile", "aa"})
	if err != nil || !p.NoBeta {
		t.Errorf("preset provider Env carries the beta key: NoBeta should be true (err %v)", err)
	}
	if providerHasBetaOff(nil, "aa") || providerHasBetaOff(mustLoad(t), "missing") {
		t.Error("unknown profile: NoBeta should be false")
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
	if code := Run([]string{"sonnet", "--profile", "aa"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if slices.Contains(f.calls[0].env, betaOff) {
		t.Error("preset provider owns the opt-out: launch must not inject it")
	}
}
