package agent

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// leadRegistry has Databricks row dev (signed in) and Anthropic ultra; claude's record pins ultra.
func leadRegistry(t *testing.T) *providers.Registry {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	vaulttest.SetRecord(t, "claude", "profile", "ultra")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// With a lead, the wanted agent's lead row is its own route and its pin is the other route.
func TestOtherRouteFollowsLead(t *testing.T) {
	rc := routeCtx{want: "codex", lead: "dev", pins: map[string]router.PinSpec{"codex": {Profile: "pin"}}}
	if rc.otherRoute(router.Candidate{Agent: "codex", Profile: "dev"}) {
		t.Error("lead row marked as another route")
	}
	if !rc.otherRoute(router.Candidate{Agent: "codex", Profile: "pin"}) {
		t.Error("pin row under a lead not marked as another route")
	}
}

// A pinned caller's own profile:model runs the bare model in its cell; buildDispatch adds the profile once.
func TestPinnedCellStripsOwnProfile(t *testing.T) {
	rc := routeCtx{want: "codex", explicitModel: true, callerModel: "dev:databricks-gpt-6-1-sol",
		pins: map[string]router.PinSpec{"codex": {Profile: "dev"}}}
	cell, skip := rc.candidateCell(router.Candidate{Agent: "codex", Profile: "dev"})
	if skip != "" || cell.Model != "databricks-gpt-6-1-sol" {
		t.Errorf("cell model %q skip %q, want databricks-gpt-6-1-sol", cell.Model, skip)
	}
}

// LeadFor: a named profile leads (D1) unless --fallback none or Pinned pins it; session and --provider always lead.
func TestLeadFor(t *testing.T) {
	leadRegistry(t)
	t.Setenv(providers.EnvSessionProfile, "dev")
	all, none := router.FallbackAll, router.FallbackNone
	cases := []struct {
		q        RouteReq
		lead     string
		explicit bool
		err      string
	}{
		{RouteReq{Agent: "codex", Fallback: all}, "", false, ""},
		{RouteReq{Agent: "codex", Profile: "dev", Fallback: all}, "dev", false, ""},
		{RouteReq{Agent: "codex", Profile: "dev", Fallback: none}, "", true, ""},
		{RouteReq{Agent: "codex", Profile: "dev", Fallback: all, Pinned: true}, "", true, ""},
		{RouteReq{Agent: "codex", Profile: LeadSession, Fallback: none}, "dev", false, ""},
		{RouteReq{Agent: "codex", Provider: "databricks", Fallback: none}, "dev", false, ""},
		{RouteReq{Agent: "codex", Profile: "dev", Lead: "ultra", Pinned: true}, "ultra", false, ""},
		{RouteReq{Agent: "codex", Model: "dev:opus", Fallback: all}, "", true, ""},
		{RouteReq{Agent: "codex", Profile: "nosuch", Fallback: none}, "", true, "nosuch"},
		{RouteReq{Agent: "codex", Profile: "nosuch", Fallback: all}, "", false, "nosuch"},
	}
	for _, c := range cases {
		lead, explicit, err := LeadFor(c.q)
		errOK := (c.err == "") == (err == nil) && (err == nil || strings.Contains(err.Error(), c.err))
		if lead != c.lead || explicit != c.explicit || !errOK {
			t.Errorf("%+v: got %q, %v, %v", c.q, lead, explicit, err)
		}
	}
}

// A preview of a profile:model leads on that model's profile; an unflagged preview has no lead.
func TestRouteLeadPreviewKeepsModelProfile(t *testing.T) {
	leadRegistry(t)
	t.Setenv(providers.EnvSessionProfile, "ultra")
	cases := []struct {
		q    RouteReq
		lead string
	}{
		{RouteReq{Agent: "codex", Fallback: router.FallbackAll, Preview: true}, ""},
		{RouteReq{Agent: "codex", Model: "ultra:opus", Fallback: router.FallbackAll, Preview: true}, "ultra"},
		{RouteReq{Agent: "codex", Model: "dev:opus", Profile: "ultra", Fallback: router.FallbackAll, Preview: true}, "ultra"},
	}
	for _, c := range cases {
		lead, explicit, err := routeLead(c.q)
		if lead != c.lead || explicit || err != nil {
			t.Errorf("%+v: got %q, %v, %v; want lead %q", c.q, lead, explicit, err, c.lead)
		}
	}
}

// The retired HARNESS_DATABRICKS_ARM env no longer moves an unflagged dispatch off its pin.
func TestRouteLeadIgnoresRetiredArmEnv(t *testing.T) {
	leadRegistry(t)
	t.Setenv("HARNESS_DATABRICKS_ARM", "dev")
	lead, explicit, err := routeLead(RouteReq{Agent: "codex", Fallback: router.FallbackAll})
	if lead != "" || explicit || err != nil {
		t.Errorf("got %q, %v, %v; want no lead", lead, explicit, err)
	}
}
