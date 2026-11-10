package copilot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// An unreadable registry hides any GitHub pin, so the launch must be refused, never run on another account's token.
func TestCopilotLaunchTokenRefusesOnRegistryLoadError(t *testing.T) {
	pinsScratch(t)
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_TOKEN", "other-account-token")
	vaulttest.Put(t, vault.Overlay, "providers", "{not json")

	tok, err := CopilotLaunchToken()
	if err == nil || tok != "" {
		t.Fatalf("CopilotLaunchToken() = (%q, %v), want an error and no token", tok, err)
	}
	if got := CopilotToken(); got != "" {
		t.Errorf("CopilotToken() = %q, want no token on a registry load error", got)
	}
}

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

// A seat checked inside StaleAfter reads unverified without a refresh; an unchecked one refreshes under the 1 s cap.
func TestCopilotUsedPctBacksOffAndCapsRefresh(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := router.MarkChecked("github·checked", "unlimited", time.Now()); err != nil {
		t.Fatal(err)
	}
	orig := RefreshAccount
	t.Cleanup(func() { RefreshAccount = orig })
	var calls []string
	RefreshAccount = func(ctx context.Context, key string) bool {
		calls = append(calls, key)
		if d, ok := ctx.Deadline(); !ok || time.Until(d) > refreshCap {
			t.Errorf("refresh of %s ran without the %v cap", key, refreshCap)
		}
		return false
	}
	for _, login := range []string{"checked", "fresh"} {
		if pct, ok := copilotUsedPct(context.Background(), login); ok {
			t.Errorf("copilotUsedPct(%s) = %v, verified; want unverified", login, pct)
		}
	}
	if len(calls) != 1 || calls[0] != "github·fresh" {
		t.Errorf("refreshed %v, want only github·fresh", calls)
	}
}

// An unreadable registry may hide a GitHub pin: the presence gate and route headroom must say so, not read it as unpinned.
