package agent

import (
	"errors"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

func TestByokRetryModelSkipsFailedAndStaysOnPool(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "byok-models", `["m-a","m-b"]`) }); err != nil {
		t.Fatal(err)
	}
	p := &providers.Provider{Name: "dbx", Provider: providers.GroupDatabricks, Preset: providers.PresetDatabricks,
		Auth: providers.Auth{Type: providers.AuthKey, Env: "X"}}
	first, note := PinnedModel("opencode", Tiers[1], Cell{}, p, BYOKModel)
	if first != "m-a" || note != "" {
		t.Fatalf("first pick = %q (%q), want m-a", first, note)
	}
	leg := Leg{repick: byokRepick("opencode", Tiers[1], Cell{}, p, "dbx:")}
	capErr := errors.New("400: max_new_tokens exceeds the model limit")
	if got := retryModel(leg, RunOptions{Model: "dbx:m-a"}, errors.New("connection reset")); got != "" {
		t.Errorf("non-capability error retried on %q", got)
	}
	if got := retryModel(leg, RunOptions{Model: "dbx:m-a"}, capErr); got != "dbx:m-b" {
		t.Errorf("capability retry = %q, want dbx:m-b", got)
	}
	schemaErr := errors.New(`Bad Request: Invalid JSON payload received. Unknown name "$schema" at 'tools[0].function_declarations[0].parameters'`)
	if got := retryModel(leg, RunOptions{Model: "dbx:m-b"}, schemaErr); got != "" {
		t.Errorf("tool-schema retry with the pool exhausted = %q, want none", got)
	}
	if router.LoadState()[router.ProviderAccount(p.Name)].BYOKSkip["opencode|m-b"].IsZero() {
		t.Error("tool-schema rejection did not mark m-b skipped for opencode")
	}
	at := router.LoadState()[router.ProviderAccount(p.Name)].BYOKSkip["opencode|m-a"]
	if time.Since(at) > time.Minute || at.IsZero() {
		t.Errorf("m-a not marked skipped for opencode: %v", at)
	}
	if got := retryModel(Leg{}, RunOptions{Model: "x"}, capErr); got != "" {
		t.Errorf("leg without a byok repick retried on %q", got)
	}
}
