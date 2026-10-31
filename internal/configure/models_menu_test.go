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
// catalogForSlot falls back to RegistryModels(p) — the provider's own configured per-slot ids, in
// models.Slots order — deterministically and without hitting the network.
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

// TestPickModelForSlotHeaderNeverAppearsInRows: pickModelForSlot's header argument names the columns;
// it must never also appear as a pickable row.
func TestPickModelForSlotHeaderNeverAppearsInRows(t *testing.T) {
	reg, sel := subscriptionSelection(t)
	origPick, origSlots := pickModels, pickSlots
	t.Cleanup(func() { pickModels, pickSlots = origPick, origSlots })
	var gotHeader [2]string
	var gotRows [][2]string
	pickModels = func(title string, header [2]string, rows [][2]string, start int) (int, error) {
		gotHeader, gotRows = header, rows
		return -1, nil
	}
	slot := models.Slots[0]
	pickModelForSlot(&dialog{reg: reg, out: discardWriter{}}, sel, slot)
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
	origPick, origSlots := pickModels, pickSlots
	t.Cleanup(func() { pickModels, pickSlots = origPick, origSlots })
	slot := models.Slots[0]
	pickModels = func(title string, header [2]string, rows [][2]string, start int) (int, error) {
		return 0, nil
	}
	pickModelForSlot(&dialog{reg: reg, out: discardWriter{}}, sel, slot)
	want := reg.Providers["max"].Models[slot]
	if got := agentpkg.GetAgentRecord("claude").Slots[strings.ToLower(slot)]; got != want {
		t.Errorf("pin(%s) = %q, want %q (rows[0]'s id)", providers.SlotPin(slot), got, want)
	}
}

// TestPickModelForSlotMinusOnePinsNothing: Back/Esc from the picker must leave every pin untouched.
func TestPickModelForSlotMinusOnePinsNothing(t *testing.T) {
	reg, sel := subscriptionSelection(t)
	origPick, origSlots := pickModels, pickSlots
	t.Cleanup(func() { pickModels, pickSlots = origPick, origSlots })
	slot := models.Slots[0]
	pickModels = func(title string, header [2]string, rows [][2]string, start int) (int, error) {
		return -1, nil
	}
	pickModelForSlot(&dialog{reg: reg, out: discardWriter{}}, sel, slot)
	if got := agentpkg.GetAgentRecord("claude").Slots[strings.ToLower(slot)]; got != "" {
		t.Errorf("pin(%s) = %q, want unset after Back/Esc", providers.SlotPin(slot), got)
	}
}

// discardWriter is an io.Writer that throws away everything written to it (dialog.out needs one when
// a test never asserts on dialog output).
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
