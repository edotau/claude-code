package copilot

import (
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// Plan is the gh login's Copilot plan (3 s, cached per gh account) plus the account a token-only source belongs to.
func (copilotEngine) Plan(_ context.Context, st providers.AuthState) providers.Subscription {
	raw := CopilotRawPlan()
	p := providers.Subscription{Raw: raw, Name: providers.CopilotPlanName(raw)}
	if st.Account == "" {
		if a, ok := copilotAuthSource(); ok && a.token != "" {
			p.Account = GitHubLogin(a.token)
		}
	}
	return p
}

// copilotTierCmd is the plan lookup seam; tests replace it.
var copilotTierCmd = func(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "gh", "api", "/copilot_internal/user").Output()
}

// copilotPlanCache holds one gh lookup per active gh account, so a sign-in or account switch mid-menu re-asks.
var copilotPlanCache = struct {
	sync.Mutex
	plans map[string]string
}{plans: map[string]string{}}

// CopilotRawPlan asks GitHub which Copilot plan the gh login holds ("individual_pro"; 3 s cap); "" when offline.
func CopilotRawPlan() string {
	account := GHActiveAccount()
	copilotPlanCache.Lock() // held across the lookup so parallel home-page builders ask gh once
	defer copilotPlanCache.Unlock()
	if v, ok := copilotPlanCache.plans[account]; ok {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var u struct {
		Plan string `json:"copilot_plan"`
	}
	if out, err := copilotTierCmd(ctx); err == nil {
		_ = json.Unmarshal(out, &u)
	}
	copilotPlanCache.plans[account] = u.Plan
	return u.Plan
}
