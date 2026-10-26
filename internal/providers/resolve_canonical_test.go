package providers

import "testing"

func TestParseTargetCanonicalAndOneM(t *testing.T) {
	reg := load(t)
	def := &Provider{Name: "plain"} // OneM=false
	other := reg.Providers["openai"]
	if other == nil {
		t.Fatal("openai provider missing from test registry")
	}
	cases := []struct {
		spec  string
		prov  *Provider
		model string
		oneM  bool
	}{
		{"claude-opus-5-5[1m]", def, "claude-opus-5-5", true},
		{"claude-opus-5-5", def, "claude-opus-5-5", false},
		{"databricks-claude-opus-5-5", def, "claude-opus-5-5", false}, // no ModelPrefix: canonical
		{"opus-5-5[1m]", def, "claude-opus-5-5", true},
		{"openai:gpt-5-nano", other, "gpt-5-nano", false},
		{"openai:databricks-claude-opus-5[1m]", other, "claude-opus-5", true},
		{"qwen3:30b", def, "qwen3:30b", false},
	}
	for _, c := range cases {
		got := ParseTarget(reg, c.spec, def)
		if got.Provider != c.prov || got.Model != c.model || got.OneM != c.oneM {
			t.Errorf("ParseTarget(%q) = %s/%q/%v; want %s/%q/%v",
				c.spec, got.Provider.Name, got.Model, got.OneM, c.prov.Name, c.model, c.oneM)
		}
	}
}

func TestClientModelKeepsTypedOneM(t *testing.T) {
	plain := &Provider{Name: "plain"}
	other := &Provider{Name: "other"}
	sel := Selection{Provider: plain, Slots: map[string]Target{
		"opus":   {Provider: plain, Model: "claude-opus-5-5", OneM: true},
		"sonnet": {Provider: plain, Model: "claude-sonnet-5"},
		"haiku":  {Provider: other, Model: "claude-haiku-4-5", OneM: true},
		"fable":  {Provider: other, Model: "gpt-5"},
	}}
	want := map[string]string{
		"opus":   "claude-opus-5-5[1m]",
		"sonnet": "claude-sonnet-5",
		"haiku":  "other:claude-haiku-4-5[1m]",
		"fable":  "other:gpt-5",
	}
	for slot, w := range want {
		if got := sel.ClientModel(slot); got != w {
			t.Errorf("ClientModel(%s) = %q; want %q", slot, got, w)
		}
	}
}

// A prefixed provider gets its wire id whichever spelling is typed; first-party keeps the canonical id.
func TestParseTargetWirePrefix(t *testing.T) {
	reg := load(t)
	qa := &Provider{Name: "databricks-qa", ModelPrefix: "databricks-"}
	sub := &Provider{Name: "subscription"}
	cases := []struct {
		spec  string
		prov  *Provider
		model string
		oneM  bool
	}{
		{"opus-5-5", qa, "databricks-claude-opus-5-5", false},
		{"claude-opus-5-5", qa, "databricks-claude-opus-5-5", false},
		{"databricks-claude-opus-5-5", qa, "databricks-claude-opus-5-5", false},
		{"opus-5-5[1m]", qa, "databricks-claude-opus-5-5", true},
		{"opus-5-5", sub, "claude-opus-5-5", false},
		{"opus-5-5[1m]", sub, "claude-opus-5-5", true},
	}
	for _, c := range cases {
		got := ParseTarget(reg, c.spec, c.prov)
		if got.Model != c.model || got.OneM != c.oneM {
			t.Errorf("%s ParseTarget(%q) = %q/%v; want %q/%v", c.prov.Name, c.spec, got.Model, got.OneM, c.model, c.oneM)
		}
	}
}
