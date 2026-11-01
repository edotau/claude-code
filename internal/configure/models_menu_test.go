package configure

import (
	"strings"
	"testing"

	agentpkg "github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/models"
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
	putRec(t, "claude", "PROFILE", "max")
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

// TestPickModelForSlotHeaderNeverAppearsInRows: pickModelForAlias's header argument names the columns;
// it must never also appear as a pickable row.
func TestPickModelForSlotHeaderNeverAppearsInRows(t *testing.T) {
	reg, sel := subscriptionSelection(t)
	origPick, origSlots := pickModels, pickAliases
	t.Cleanup(func() { pickModels, pickAliases = origPick, origSlots })
	var gotHeader [2]string
	var gotRows [][2]string
	pickModels = func(title string, header [2]string, rows [][2]string, start int) (int, error) {
		gotHeader, gotRows = header, rows
		return -1, nil
	}
	alias := models.AliasNames[0]
	pickModelForAlias(&dialog{reg: reg, out: discardWriter{}}, sel, alias)
	if gotHeader[0] != "ID" {
		t.Fatalf("header = %v, want the ID/Model/Use case/Capabilities header", gotHeader)
	}
	for _, r := range gotRows {
		if r == gotHeader {
			t.Fatalf("header row %v leaked into the pickable rows %v", gotHeader, gotRows)
		}
	}
	if len(gotRows) == 0 {
		t.Fatal("rows empty: subscription's registry-models fallback should have produced at least one row")
	}
}

// TestPickModelForSlotPicksFirstPinsIt: idx 0 from the picker pins rows[0]'s id — no -1 offset, since
// the header is no longer a pickable row (tui.PickTable strips it before the index reaches here).
func TestPickModelForSlotPicksFirstPinsIt(t *testing.T) {
	reg, sel := subscriptionSelection(t)
	origPick, origSlots := pickModels, pickAliases
	t.Cleanup(func() { pickModels, pickAliases = origPick, origSlots })
	alias := models.AliasNames[0]
	pickModels = func(title string, header [2]string, rows [][2]string, start int) (int, error) {
		return 0, nil
	}
	pickModelForAlias(&dialog{reg: reg, out: discardWriter{}}, sel, alias)
	want := reg.Providers["max"].Models[alias]
	if got := agentpkg.GetAgentRecord("claude").Aliases[strings.ToLower(alias)]; got != want {
		t.Errorf("alias %s = %q, want %q (rows[0]'s id)", alias, got, want)
	}
}

// TestPickModelForSlotMinusOnePinsNothing: Back/Esc from the picker must leave every pin untouched.
func TestPickModelForSlotMinusOnePinsNothing(t *testing.T) {
	reg, sel := subscriptionSelection(t)
	origPick, origSlots := pickModels, pickAliases
	t.Cleanup(func() { pickModels, pickAliases = origPick, origSlots })
	alias := models.AliasNames[0]
	pickModels = func(title string, header [2]string, rows [][2]string, start int) (int, error) {
		return -1, nil
	}
	pickModelForAlias(&dialog{reg: reg, out: discardWriter{}}, sel, alias)
	if got := agentpkg.GetAgentRecord("claude").Aliases[strings.ToLower(alias)]; got != "" {
		t.Errorf("alias %s = %q, want unset after Back/Esc", alias, got)
	}
}

// discardWriter is an io.Writer that throws away everything written to it (dialog.out needs one when
// a test never asserts on dialog output).
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
