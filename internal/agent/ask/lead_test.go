package ask

import (
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// D1: a named --profile leads with the ladder unless --fallback none pins it to one hop.
func TestResolveLeadNamedProfileLeads(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"qa":{"preset":"databricks",`+
		`"host":"https://qa.cloud.databricks.com","auth":{"type":"pat"}}}}}`)
	cases := []struct {
		args               []string
		wantLead, wantProv string
	}{
		{[]string{"--profile", "qa"}, "qa", ""},
		{[]string{"--profile", "qa", "--fallback", "agents"}, "qa", ""},
		{[]string{"--profile", "qa", "--fallback", "none"}, "", "qa"},
	}
	for _, c := range cases {
		fs, f := askFlagSet()
		if err := fs.Parse(c.args); err != nil {
			t.Fatal(err)
		}
		r := &askRun{fs: fs, f: f, ag: "codex"}
		if code, done := r.resolveLead(); done {
			t.Fatalf("%v: resolveLead exit %d", c.args, code)
		}
		if r.lead != c.wantLead || f.provider != c.wantProv {
			t.Errorf("%v: lead %q profile %q, want lead %q profile %q", c.args, r.lead, f.provider, c.wantLead, c.wantProv)
		}
	}
}
