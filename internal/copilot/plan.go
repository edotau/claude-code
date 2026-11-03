package copilot

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// Plan is the Copilot plan of the account copilot signs in as (its own token, 3 s, cached per account).
func (copilotEngine) Plan(_ context.Context, st providers.AuthState) providers.Subscription {
	a, ok := copilotAuthSource()
	tok := ""
	if ok {
		tok, _ = copilotToken(a)
	}
	account := cmp.Or(st.Account, a.Account)
	if account == "" && tok != "" {
		account = GitHubLogin(tok)
	}
	raw := CopilotRawPlan(account, tok)
	return providers.Subscription{Raw: raw, Name: providers.CopilotPlanName(raw), Account: account}
}

// copilotTierCmd is the plan lookup seam; tests replace it. A non-empty token asks as that account, not gh's active one.
var copilotTierCmd = func(ctx context.Context, token string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "/copilot_internal/user")
	if token != "" {
		cmd.Env = append(os.Environ(), "GH_TOKEN="+token)
	}
	return cmd.Output()
}

// copilotPlanMemo holds one lookup per account, so a sign-in or account switch mid-menu re-asks.
var copilotPlanMemo authMemo

// CopilotRawPlan asks GitHub which Copilot plan account holds ("individual_pro"; 3 s cap); "" when offline.
func CopilotRawPlan(account, token string) string {
	return copilotPlanMemo.do(cmp.Or(account, GHActiveAccount()), func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var u struct {
			Plan string `json:"copilot_plan"`
		}
		if out, err := copilotTierCmd(ctx, token); err == nil {
			_ = json.Unmarshal(out, &u)
		}
		return u.Plan
	})
}
