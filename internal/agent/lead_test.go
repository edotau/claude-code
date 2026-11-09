package agent

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// leadRegistry has Databricks row dev (signed in) and Anthropic max; claude's record pins max.
func leadRegistry(t *testing.T) *providers.Registry {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks",`+
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	vaulttest.SetRecord(t, "claude", "profile", "max")
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestResolveLead(t *testing.T) {
	reg := leadRegistry(t)
	cases := []struct{ env, agent, profile, provider, want, err string }{
		{"dev", "codex", LeadSession, "", "dev", ""},
		{"", "codex", LeadSession, "", "max", ""},
		{"", "codex", "dev", "databricks", "dev", ""},
		{"", "codex", "max", "databricks", "", "not a databricks profile"},
		{"", "codex", LeadSession, "databricks", "dev", ""},
		{"", "codex", "", "github", "", "no signed-in github profile serves codex"},
		{"", "codex", "nosuch", "", "", "not in the registry"},
		{"dev", "gemini", LeadSession, "", "", ""},
		{"dev", "gemini", "dev", "", "dev", ""},
	}
	for _, c := range cases {
		t.Setenv(providers.EnvSessionProfile, c.env)
		got, err := ResolveLead(reg, c.agent, c.profile, c.provider)
		if got != c.want || (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%+v: got %q, %v", c, got, err)
		}
	}
}

// HARNESS_DATABRICKS_ARM moves every non-claude agent (gemini too) onto Databricks from any session; unset keeps pins.
func TestArmLead(t *testing.T) {
	reg := leadRegistry(t)
	cases := []struct{ session, arm, agent, want, err string }{
		{"max", "", "gemini", "", ""},
		{"max", "off", "codex", "", ""},
		{"max", "session", "gemini", "dev", ""},
		{"dev", "session", "codex", "dev", ""},
		{"max", "dev", "codex", "dev", ""},
		{"max", "dev", "claude", "", ""},
		{"dev", "max", "codex", "", "not a databricks profile"},
		{"max", "nosuch", "codex", "", "not in the registry"},
	}
	for _, c := range cases {
		t.Setenv(providers.EnvSessionProfile, c.session)
		t.Setenv(EnvDatabricksArm, c.arm)
		got, err := ArmLead(reg, c.agent)
		if got != c.want || (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%+v: got %q, %v", c, got, err)
		}
	}
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
		{RouteReq{Agent: "codex", Profile: "dev", Lead: "max", Pinned: true}, "max", false, ""},
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

// A preview of a profile:model names its own route: the arm leads only an unflagged preview, never that one.
func TestRouteLeadPreviewKeepsModelProfileOverArm(t *testing.T) {
	leadRegistry(t)
	t.Setenv(providers.EnvSessionProfile, "max")
	t.Setenv(EnvDatabricksArm, "dev")
	cases := []struct {
		q    RouteReq
		lead string
	}{
		{RouteReq{Agent: "codex", Fallback: router.FallbackAll, Preview: true}, "dev"},
		{RouteReq{Agent: "codex", Model: "max:opus", Fallback: router.FallbackAll, Preview: true}, ""},
	}
	for _, c := range cases {
		lead, explicit, err := routeLead(c.q)
		if lead != c.lead || explicit || err != nil {
			t.Errorf("%+v: got %q, %v, %v; want lead %q", c.q, lead, explicit, err, c.lead)
		}
	}
}
