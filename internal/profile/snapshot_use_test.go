package profile

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestSnapshotUseOwnAppliesVendorModelWithoutProvider(t *testing.T) {
	seed(t, "")
	if err := providers.SetPins(map[string]string{providers.PinCopilotModel: "vendor-m"}); err != nil {
		t.Fatal(err)
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	set, err := Snapshot(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save("own", set, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Use(true, "own"); err != nil {
		t.Fatal(err)
	}
	if reg, err = providers.Load(); err != nil {
		t.Fatal(err)
	}
	if b := reg.ResolveAgent(providers.AgentRequest{Agent: "copilot"}); b.Model != "vendor-m" || b.Profile != "own" {
		t.Errorf("binding = %+v, want Model vendor-m from profile own", b)
	}
}

func TestMigrateNotesShellProviderNamingRemovedTwin(t *testing.T) {
	seed(t, `{"providers":{`+twinPair+`}}`)
	t.Setenv(providers.PinProvider, "databricks-dev-oai")
	_, _, notes, err := Migrate(false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), providers.PinProvider) {
		t.Errorf("no HARNESS_PROVIDER note in %q", notes)
	}
}
