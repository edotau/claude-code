package ask

import "testing"

func TestActionCheckRefusals(t *testing.T) {
	for name, f := range map[string]*askFlags{
		"gateway own": {action: "gateway", actionOwn: []string{"a.go"}, format: "json"},
		"waves":       {action: "gate", waves: "plan.md", format: "json"},
	} {
		if actionCheck(f) == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
	if err := actionCheck(&askFlags{action: "gateway", format: "json"}); err != nil {
		t.Errorf("plain gateway: %v", err)
	}
}
