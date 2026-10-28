package providers

import (
	"strings"
	"testing"
)

const nestedTestDoc = `{"default":"work","providers":{
 "anthropic":{"work":{"protocol":"anthropic","base_url":"https://api.anthropic.com","auth":{"type":"key"}}},
 "openai":{"scratch":{"protocol":"openai","base_url":"https://api.openai.com/v1","auth":{"type":"key"}}}}}`

func TestParseNestedByProvider(t *testing.T) {
	reg, err := Parse(defaultsJSON, []byte(nestedTestDoc))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"work": "anthropic", "scratch": "openai", "max": "anthropic", "personal": "github"} {
		if p := reg.Providers[name]; p == nil || p.Provider != want || p.Name != name {
			t.Errorf("profile %q = %+v, want provider %q", name, p, want)
		}
	}
	if reg.Default != "work" {
		t.Errorf("default = %q", reg.Default)
	}
	if got := reg.ProfilesOf("anthropic"); strings.Join(got, ",") != "max,anthropic,work" {
		t.Errorf("ProfilesOf(anthropic) = %v", got)
	}
}

func TestBuiltinProfileNames(t *testing.T) {
	reg, err := Parse(defaultsJSON, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"max": "anthropic", "anthropic": "anthropic", "personal": "github", "chatgpt": "openai",
		"openai": "openai", "antigravity": "google", "nvidia": "nvidia"}
	if len(reg.Providers) != len(want) {
		t.Errorf("built-ins = %v, want exactly %d profiles (no ollama)", reg.Names(), len(want))
	}
	for n, prov := range want {
		if p := reg.Providers[n]; p == nil || p.Provider != prov {
			t.Errorf("built-in %q = %+v, want provider %q", n, p, prov)
		}
	}
}

func TestDuplicateProfileRejected(t *testing.T) {
	cases := map[string]string{
		"two providers in one doc": `{"providers":{"anthropic":{"dup":{"protocol":"anthropic","base_url":"https://x","auth":{"type":"none"}}},` +
			`"openai":{"dup":{"protocol":"openai","base_url":"https://y","auth":{"type":"none"}}}}}`,
		"overlay profile owned by another provider": `{"providers":{"openai":{"max":{"protocol":"openai","base_url":"https://y","auth":{"type":"none"}}}}}`,
	}
	for name, doc := range cases {
		_, err := Parse(defaultsJSON, []byte(doc))
		if err == nil || !strings.Contains(err.Error(), "unique across providers") {
			t.Errorf("%s: err = %v, want a unique-across-providers rejection", name, err)
		}
	}
}

func TestOverlayWritesNestedAndAcceptsFlatView(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := SetProviderFields("qa", map[string]any{
		"preset": "databricks", "host": "https://dbc-3.cloud.databricks.com", "cliProfile": "qa",
	}); err != nil {
		t.Fatal(err)
	}
	doc := readOverlay(t, "providers")
	if !strings.Contains(doc, `"databricks": {`) || !strings.Contains(doc, `"qa": {`) {
		t.Fatalf("overlay is not nested by provider:\n%s", doc)
	}
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if reg.Providers["qa"].Provider != "databricks" {
		t.Errorf("qa provider = %q", reg.Providers["qa"].Provider)
	}
	if got := DatabricksProfiles(reg); len(got) != 1 || got[0] != "qa" {
		t.Errorf("DatabricksProfiles = %v", got)
	}
}

func TestWireGatewaySpellingRead(t *testing.T) {
	for _, w := range []string{"ai-gateway"} {
		doc := `{"providers":{"databricks":{"gw":{"preset":"databricks","host":"https://h.cloud.databricks.com","options":{"wire":"` + w + `"}}}}}`
		reg, err := Parse(defaultsJSON, []byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		p := reg.Providers["gw"]
		if p.Options.Wire != WireAIGateway || p.WireModel("claude-opus-5-5") != "system.ai.claude-opus-5-5" {
			t.Errorf("wire %q: %q, %q", w, p.Options.Wire, p.WireModel("claude-opus-5-5"))
		}
	}
}

func TestRoutingCellsAcceptClaudeIDsAndAliases(t *testing.T) {
	for key, ok := range map[string]bool{"opus": true, "claude-opus-5-5": true, "claude-haiku-5-5": true, "claude-fable-5-1": false, "bogus": false} {
		doc := `{"routing":{"cells":{"claude":{"` + key + `":{"model":"claude-opus-5-5"}}}}}`
		_, err := Parse(defaultsJSON, []byte(doc))
		if (err == nil) != ok {
			t.Errorf("cells key %q: err = %v, want accepted=%v", key, err, ok)
		}
	}
}

func TestDuplicateOpencodeIDRejected(t *testing.T) {
	doc := `{"providers":{"openai":{"openai":{"protocol":"openai","base_url":"https://api.openai.com/v1","auth":{"type":"key"},"opencodeID":"openai"}}}}`
	if _, err := Parse(defaultsJSON, []byte(doc)); err == nil || !strings.Contains(err.Error(), "both claim opencodeID") {
		t.Errorf("err = %v, want an opencodeID ownership rejection", err)
	}
	reg, _ := Parse(defaultsJSON, nil)
	if reg.Providers["chatgpt"].OpencodeID != "openai" || reg.Providers["openai"].OpencodeID != "" {
		t.Error("chatgpt must own opencodeID openai in the built-ins")
	}
}

func TestSameModelIgnoresOneMAndSpelling(t *testing.T) {
	p := Provider{Models: map[string]string{"opus": "claude-opus-5-5[1m]", "sonnet": "databricks-claude-sonnet-5-5"}}
	if got := p.SlotOf("claude-opus-5-5"); got != "opus" {
		t.Errorf("SlotOf(plain) with a [1m] table = %q", got)
	}
	if got := p.SlotOf("claude-sonnet-5.5"); got != "sonnet" {
		t.Errorf("SlotOf(dotted) = %q", got)
	}
	if tgt, ok := Demote(&p, "claude-opus-5-5"); !ok || tgt.Model != "databricks-claude-sonnet-5-5" {
		t.Errorf("Demote = %+v, %v", tgt, ok)
	}
}

func TestWireModelNoDoublePrefix(t *testing.T) {
	p := Provider{Preset: PresetDatabricks, Options: Options{Wire: WireAIGateway}}
	for _, in := range []string{"system.ai.claude-opus-5-5", "databricks-claude-opus-5-5", "claude-opus-5.5", "opus"} {
		if got := p.WireModel(in); got != "system.ai.claude-opus-5-5" {
			t.Errorf("WireModel(%q) = %q", in, got)
		}
	}
}

func TestProfilesOfFollowsDeclaredOrder(t *testing.T) {
	reg := mustParse(t, `{"providers":{"databricks":{
"dev":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
"qa":{"preset":"databricks","host":"https://h.cloud.databricks.com"},
"prod":{"preset":"databricks","host":"https://h.cloud.databricks.com"}}}}`)
	if got := strings.Join(DatabricksProfiles(reg), ","); got != "dev,qa,prod" {
		t.Errorf("DatabricksProfiles = %s, want dev,qa,prod", got)
	}
}
