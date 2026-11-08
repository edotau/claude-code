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
