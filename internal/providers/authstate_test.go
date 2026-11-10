package providers

import "testing"

// TestAccountLabelUltra: a 20x claude.ai login shows as ultra; other tiers, auths and profiles keep their names.
func TestAccountLabelUltra(t *testing.T) {
	reg := &Registry{Providers: map[string]*Provider{
		"max":       {Name: "max", Provider: "anthropic", Auth: Auth{Type: AuthCLI}},
		"anthropic": {Name: "anthropic", Provider: "anthropic", Auth: Auth{Type: AuthKey}},
		"qa":        {Name: "qa", Provider: "databricks"},
	}}
	const ultra, max5 = "default_claude_max_20x", "default_claude_max_5x"
	cases := []struct{ account, tier, want string }{
		{"anthropic·max", ultra, "anthropic·ultra"},
		{"anthropic·max", max5, "anthropic·max"},
		{"anthropic·max", "", "anthropic·max"},
		{"anthropic·anthropic", ultra, "anthropic·anthropic"}, // API key: not the subscription
		{"databricks·qa", ultra, "databricks·qa"},
		{"unknown·row", ultra, "unknown·row"},
	}
	for _, c := range cases {
		if got := AccountLabel(reg, c.account, c.tier); got != c.want {
			t.Errorf("AccountLabel(%q, %q) = %q, want %q", c.account, c.tier, got, c.want)
		}
	}
}
