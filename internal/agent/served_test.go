package agent

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

const servedRow = `{"routing":{"servedAware":%s},"providers":{"databricks":{"dev":{"preset":"databricks",` +
	`"host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`

// servedEnv seeds a hermetic vault with a signed-in Databricks row "dev", the switch on or off, a two-model Responses
// menu and (when cache is non-nil) a fresh listing of it; it returns the loaded registry.
func servedEnv(t *testing.T, on string, cache map[string]string) *providers.Registry {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", strings.Replace(servedRow, "%s", on, 1))
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	menu := []providers.OpencodeModel{{ID: "m-a", Label: "A", Responses: true}, {ID: "m-b", Label: "B", Responses: true}}
	if err := providers.SetOpencodeMenu(menu); err != nil {
		t.Fatal(err)
	}
	if cache != nil {
		s := preflight.Served{Profile: "dev", FetchedAt: time.Now()}
		for _, id := range []string{"m-a", "m-b"} {
			if r, ok := cache[id]; ok {
				s.Models = append(s.Models, preflight.ServedModel{ID: id, Ready: r})
			}
		}
		if err := preflight.SaveServed(s); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

var servedCand = router.Candidate{Agent: "codex", Profile: "dev", Account: "databricks·dev"}

// profileCell refuses an exact model the fresh listing has NOT_READY, with the new message.
func TestProfileCellRejectsNotReadyExactModel(t *testing.T) {
	reg := servedEnv(t, "true", map[string]string{"m-a": "NOT_READY", "m-b": "READY"})
	_, why := routeCtx{reg: reg, exact: "m-a"}.profileCell(servedCand, router.Cell{})
	if !strings.Contains(why, "m-a is not a READY endpoint on dev") {
		t.Fatalf("skip reason = %q, want the not-READY message", why)
	}
	if cell, why := (routeCtx{reg: reg, exact: "m-b"}).profileCell(servedCand, router.Cell{}); why != "" || cell.Model != "m-b" {
		t.Errorf("READY m-b = %+v, %q, want accepted", cell, why)
	}
}

// a cold cache accepts the exact model on the static Responses check, even with the switch on.
func TestProfileCellAcceptsStaticallyOnColdCache(t *testing.T) {
	reg := servedEnv(t, "true", nil)
	cell, why := routeCtx{reg: reg, exact: "m-a"}.profileCell(servedCand, router.Cell{})
	if why != "" || cell.Model != "m-a" {
		t.Fatalf("cold cache = %+v, %q, want m-a accepted", cell, why)
	}
}

// ENDPOINT_NOT_FOUND marks the model gone and returns the next READY Responses model.
func TestServedRepickOnEndpointNotFound(t *testing.T) {
	quietStderr(t)
	reg := servedEnv(t, "true", map[string]string{"m-a": "READY", "m-b": "READY"})
	leg := Leg{servedRepick: routeCtx{reg: reg}.servedRepickFor(servedCand)}
	if leg.servedRepick == nil {
		t.Fatal("codex on a Databricks profile with the switch on must get a repick")
	}
	err := errors.New("RESOURCE_DOES_NOT_EXIST: ENDPOINT_NOT_FOUND: no endpoint m-a")
	if got := retryModel(leg, Request{Model: "dev:m-a"}, err); got != "dev:m-b" {
		t.Fatalf("repick = %q, want dev:m-b", got)
	}
	if at := router.LoadState()[router.ProviderAccount("dev")].Unserved["m-a"]; at.IsZero() {
		t.Error("m-a was not marked unserved")
	}
	if got := retryModel(leg, Request{Model: "dev:m-b"}, err); got != "" {
		t.Errorf("second failure repicked %q, want none left", got)
	}
}

// an error that is not an endpoint-gone answer neither repicks nor marks.
func TestServedRepickIgnoresUnrelatedError(t *testing.T) {
	reg := servedEnv(t, "true", map[string]string{"m-a": "READY", "m-b": "READY"})
	leg := Leg{servedRepick: routeCtx{reg: reg}.servedRepickFor(servedCand)}
	if got := retryModel(leg, Request{Model: "dev:m-a"}, errors.New("connection reset")); got != "" {
		t.Errorf("unrelated error repicked %q", got)
	}
	if at := router.LoadState()[router.ProviderAccount("dev")].Unserved["m-a"]; !at.IsZero() {
		t.Error("an unrelated error must not mark m-a")
	}
}

// an exact --model, a non-codex leg and a switch-off run get no served repick.
func TestServedRepickNotOfferedWhereItWouldMislead(t *testing.T) {
	reg := servedEnv(t, "true", map[string]string{"m-a": "READY", "m-b": "READY"})
	if (routeCtx{reg: reg, exact: "m-a"}.servedRepickFor(servedCand)) != nil {
		t.Error("exact --model must not repick")
	}
	copilot := servedCand
	copilot.Agent = "copilot"
	if (routeCtx{reg: reg}.servedRepickFor(copilot)) != nil {
		t.Error("a non-codex leg must not get a served repick")
	}
	off := servedEnv(t, "false", nil)
	if (routeCtx{reg: off}.servedRepickFor(servedCand)) != nil {
		t.Error("switch off must not repick")
	}
}

// with both repicks set, a capability error still takes the byok pool path.
func TestByokRetryModelKeepsCapabilityPathWithServedRepick(t *testing.T) {
	leg := Leg{
		repick:       func(string) string { return "dev:pool-next" },
		servedRepick: func(string) string { return "dev:served-next" },
	}
	if got := retryModel(leg, Request{Model: "dev:m"}, errors.New("400: max_new_tokens too large")); got != "dev:pool-next" {
		t.Errorf("capability error = %q, want the byok pick", got)
	}
}
