// tiers.go — the subscription tier each agent's own sign-in reports, shown as "subscription (Max 20x)".
package configure

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

var maxTierRE = regexp.MustCompile(`max_(\d+x)`)

// ClaudeTier names the claude.ai plan: the rate-limit tier in ~/.claude.json ("Max 20x") beats the bare plan ("Max").
func ClaudeTier(plan string) string {
	if m := maxTierRE.FindStringSubmatch(ClaudeRateTier()); m != nil {
		return "Max " + m[1]
	}
	return title(strings.Trim(plan, " ()"))
}

// ClaudeRateTier is the raw rate-limit tier ~/.claude.json records ("default_claude_max_20x"), user before org.
func ClaudeRateTier() string {
	b, err := os.ReadFile(paths.ClaudeJSON())
	if err != nil {
		return ""
	}
	var cfg struct {
		Account struct {
			Org  string `json:"organizationRateLimitTier"`
			User string `json:"userRateLimitTier"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	return cmp.Or(cfg.Account.User, cfg.Account.Org)
}

// CodexTier is codex's ChatGPT plan ("Plus", "Pro"), read offline.
func CodexTier() string { return title(codex.CodexPlan()) }

// copilotTierCmd is the plan lookup seam; tests replace it.
var copilotTierCmd = func(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "gh", "api", "/copilot_internal/user").Output()
}

// CopilotTier is the Copilot plan's sold name ("Pro+"); the raw copilot_plan when GitHub adds an unknown one.
func CopilotTier() string { return providers.CopilotPlanName(CopilotRawPlan()) }

// copilotPlanCache holds one gh lookup per active gh account, so a sign-in or account switch mid-menu re-asks.
var copilotPlanCache = struct {
	sync.Mutex
	plans map[string]string
}{plans: map[string]string{}}

// CopilotRawPlan asks GitHub which Copilot plan the gh login holds ("individual_pro"; 3 s cap); "" when offline.
func CopilotRawPlan() string {
	account := copilot.GHActiveAccount()
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

func title(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// WithTier renders a mode with its tier when one is known: "subscription (Max 20x)".
func WithTier(mode, tier string) string {
	if tier == "" {
		return mode
	}
	return mode + " (" + tier + ")"
}
