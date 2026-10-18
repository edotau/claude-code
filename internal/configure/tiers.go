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
	if b, err := os.ReadFile(paths.ClaudeJSON()); err == nil {
		var cfg struct {
			Account struct {
				Org  string `json:"organizationRateLimitTier"`
				User string `json:"userRateLimitTier"`
			} `json:"oauthAccount"`
		}
		if json.Unmarshal(b, &cfg) == nil {
			for _, t := range []string{cfg.Account.User, cfg.Account.Org} {
				if m := maxTierRE.FindStringSubmatch(t); m != nil {
					return "Max " + m[1]
				}
			}
		}
	}
	return title(strings.Trim(plan, " ()"))
}

// codexTier is codex's ChatGPT plan ("Plus", "Pro"), read offline.
func codexTier() string { return title(vendors.CodexPlan()) }

// copilotPlans maps GitHub's copilot_plan values to the plan names GitHub sells.
var copilotPlans = map[string]string{
	"free": "Free", "individual": "Pro", "individual_pro": "Pro+", "business": "Business", "enterprise": "Enterprise",
}

// copilotTierCmd is the plan lookup seam; tests replace it.
var copilotTierCmd = func(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "gh", "api", "/copilot_internal/user").Output()
}

// copilotTier asks GitHub which Copilot plan the gh login holds (3 s cap); "" when gh is absent or offline.
func copilotTier() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := copilotTierCmd(ctx)
	if err != nil {
		return ""
	}
	var u struct {
		Plan string `json:"copilot_plan"`
	}
	if json.Unmarshal(out, &u) != nil || u.Plan == "" {
		return ""
	}
	if name, ok := copilotPlans[u.Plan]; ok {
		return name
	}
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
