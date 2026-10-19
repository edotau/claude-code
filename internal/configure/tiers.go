// tiers.go — the subscription tier each agent's own sign-in reports, shown as "subscription (Max 20x)".
package configure

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vendors"
)

var maxTierRE = regexp.MustCompile(`max_(\d+x)`)

// claudeTier names the claude.ai plan: the rate-limit tier in ~/.claude.json ("Max 20x") beats the bare plan ("Max").
func claudeTier(plan string) string {
	if m := maxTierRE.FindStringSubmatch(claudeRateTier()); m != nil {
		return "Max " + m[1]
	}
	return title(strings.Trim(plan, " ()"))
}

// claudeRateTier is the raw rate-limit tier ~/.claude.json records ("default_claude_max_20x"), user before org.
func claudeRateTier() string {
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
	return cmpOr(cfg.Account.User, cfg.Account.Org)
}

// codexTier is codex's ChatGPT plan ("Plus", "Pro"), read offline.
func codexTier() string { return title(vendors.CodexPlan()) }

// copilotTierCmd is the plan lookup seam; tests replace it.
var copilotTierCmd = func(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "gh", "api", "/copilot_internal/user").Output()
}

// copilotTier is the Copilot plan's sold name ("Pro+"); the raw copilot_plan when GitHub adds an unknown one.
func copilotTier() string { return vendors.CopilotPlanName(copilotRawPlan()) }

// copilotPlanCache holds the one gh lookup per run; the plan does not change while configure is open.
var copilotPlanCache = map[string]string{}

// copilotRawPlan asks GitHub which Copilot plan the gh login holds ("individual_pro"; 3 s cap); "" when offline.
func copilotRawPlan() string {
	if v, ok := copilotPlanCache["plan"]; ok {
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
	copilotPlanCache["plan"] = u.Plan
	return u.Plan
}

func title(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// withTier renders a mode with its tier when one is known: "subscription (Max 20x)".
func withTier(mode, tier string) string {
	if tier == "" {
		return mode
	}
	return mode + " (" + tier + ")"
}
