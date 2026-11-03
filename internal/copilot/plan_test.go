package copilot

import (
	"context"
	"errors"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func TestCopilotTier(t *testing.T) {
	orig := copilotTierCmd
	t.Cleanup(func() { copilotTierCmd = orig })
	cases := []struct {
		name, out string
		err       error
		want      string
	}{
		{"individual", `{"copilot_plan":"individual"}`, nil, "Pro"},
		{"individual_pro", `{"copilot_plan":"individual_pro"}`, nil, "Pro+"},
		{"free", `{"copilot_plan":"free"}`, nil, "Free"},
		{"business", `{"copilot_plan":"business"}`, nil, "Business"},
		{"enterprise", `{"copilot_plan":"enterprise"}`, nil, "Enterprise"},
		{"unknown passes through", `{"copilot_plan":"new_tier"}`, nil, "new_tier"},
		{"empty plan", `{"copilot_plan":""}`, nil, ""},
		{"bad json", `nope`, nil, ""},
		{"command error", ``, errors.New("gh: not found"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clear(copilotPlanMemo.m)
			copilotTierCmd = func(context.Context, string) ([]byte, error) { return []byte(c.out), c.err }
			if got := providers.CopilotPlanName(CopilotRawPlan("", "")); got != c.want {
				t.Errorf("copilotTier = %q, want %q", got, c.want)
			}
		})
	}
}
