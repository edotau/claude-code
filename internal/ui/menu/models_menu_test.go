package menu

import (
	"bufio"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// subscriptionSelection loads the real registry and resolves the subscription provider (passthrough,
// no live token): ListModels fails instantly with ErrPassthrough (no network dial at all), so
// catalogForAlias falls back to RegistryModels(p) — the provider's own configured per-alias ids, in
// models.AliasNames order — deterministically and without hitting the network.
func subscriptionSelection(t *testing.T) (*providers.Registry, providers.Selection) {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	putRec(t, "claude", "PROFILE", "ultra")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	reg, err := providers.Load()
	if err != nil {
		t.Skip(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Skip(err)
	}
	return reg, sel
}

// stubCatalogFn swaps catalogFn for a fixed served-model list.
func stubCatalogFn(t *testing.T, infos []providers.ModelInfo) {
	t.Helper()
	orig := catalogFn
	t.Cleanup(func() { catalogFn = orig })
	catalogFn = func(context.Context, *providers.Registry, *providers.Provider, string) ([]providers.ModelInfo, string) {
		return infos, ""
	}
}

// TestOfferModelsPinsServedModelAudited: after a profile is chosen, its served models are listed per alias and the
// pick lands as the claude alias pin (unqualified for the session profile, "profile:id" for another).
func TestOfferModelsPinsServedModelAudited(t *testing.T) {
	reg, _ := subscriptionSelection(t)
	stubCatalogFn(t, []providers.ModelInfo{{ID: "m-one", Name: "One"}, {ID: "m-two", Name: "Two", UseCase: "chat"}})
	other := ""
	for _, n := range reg.Names() {
		if n != "ultra" {
			other = n
			break
		}
	}
	offer := func(profile, answers string) {
		in := strings.NewReader(answers) // alias 1 (opus) → model 2 → blank leaves the page
		d := &dialog{reg: reg, o: Options{In: in}, br: bufio.NewReader(in), out: discardWriter{}}
		offerModels(d, profile)
	}
	offer("ultra", "1\n2\n\n")
	if got := vault.GetAgentRecord("claude").Aliases["opus"]; got != "m-two" {
		t.Fatalf("session-profile opus pin = %q, want m-two", got)
	}
	if ev, _ := audit.Read(time.Time{}); !slices.ContainsFunc(ev, func(e audit.Event) bool {
		return e.Action == audit.ModelPin && e.Resource == "claude.aliases.opus"
	}) {
		t.Fatal("no model-pin audit event for the opus pick")
	}
	offer(other, "1\n1\n\n")
	if got := vault.GetAgentRecord("claude").Aliases["opus"]; got != other+":m-one" {
		t.Fatalf("cross-profile opus pin = %q, want %s:m-one", got, other)
	}
}

func TestUnpinAllModelsMarksDialogChanged(t *testing.T) {
	reg := menuTestEnv(t)
	for _, alias := range []string{"opus", "sonnet", "haiku", "fable"} {
		vaulttest.SetRecord(t, "claude", "aliases."+alias, "pinned-"+alias)
	}
	d := &dialog{reg: reg, out: discardWriter{}}
	unpinAllModels(d)
	if got := vault.GetAgentRecord("claude"); len(got.Aliases) != 0 || got.Profile != "ultra" {
		t.Fatalf("unpin all must clear aliases and retain the profile: %+v", got)
	}
	if !d.changed {
		t.Fatal("unpin all must trigger config regeneration")
	}
}
