package providers

import "testing"

// TestSetProviderModelSeedsBuiltinEntry: a provider absent from the overlay keeps its built-in auth/route/opencodeID
// when one slot is changed (an overlay entry replaces the built-in whole, so a models-only entry would break it).
func TestSetProviderModelSeedsBuiltinEntry(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SetProviderModel("personal", "haiku", "gpt-x-mini"); err != nil {
		t.Fatal(err)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := reg.Providers["personal"]
	if p.Models["haiku"] != "gpt-x-mini" || p.Models["opus"] == "" || p.OpencodeID != "github-copilot" || p.Auth.Login == "" {
		t.Errorf("entry lost built-in fields: %+v", p)
	}
}
