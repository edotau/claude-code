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
	first, note := router.PinnedModel("opencode", router.Tiers[1], router.Cell{}, p, router.BYOKModel)
	if first != "m-a" || note != "" {
		t.Fatalf("first pick = %q (%q), want m-a", first, note)
	}
	leg := Leg{repick: byokRepick("opencode", router.Tiers[1], router.Cell{}, p, "dbx:")}
	capErr := errors.New("400: max_new_tokens exceeds the model limit")
	if got := byokRetryModel(leg, Request{Model: "dbx:m-a"}, errors.New("connection reset")); got != "" {
		t.Errorf("non-capability error retried on %q", got)
	}
	if got := byokRetryModel(leg, Request{Model: "dbx:m-a"}, capErr); got != "dbx:m-b" {
		t.Errorf("capability retry = %q, want dbx:m-b", got)
	}
	at := router.LoadState()[router.ProviderAccount(p.Name)].BYOKSkip["opencode|m-a"]
	if time.Since(at) > time.Minute || at.IsZero() {
		t.Errorf("m-a not marked skipped for opencode: %v", at)
	}
	if got := byokRetryModel(Leg{}, Request{Model: "x"}, capErr); got != "" {
		t.Errorf("leg without a byok repick retried on %q", got)
	}
}
