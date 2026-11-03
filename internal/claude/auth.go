// auth.go — claude's agent.Authenticator: the claude.ai login read from `claude auth status` and ~/.claude.json.
package claude

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

var _ agent.Authenticator = claudeAgent

// claudeProbe is `claude auth status`; a seam so tests stay off the real CLI.
var claudeProbe = providers.ClaudeLoginProbe

var maxTierRE = regexp.MustCompile(`max_(\d+x)`)

// AuthStatus is the claude.ai login from `claude auth status`; a failed probe sets Err (unknown, not logged out).
func (claudeEngine) AuthStatus() providers.AuthState {
	ok, plan, err := claudeProbe()
	if err != nil {
		return providers.AuthState{Err: err}
	}
	if !ok {
		return providers.AuthState{}
	}
	return providers.AuthState{Present: true, Source: "claude.ai login", Method: providers.AuthCLI, PlanHint: strings.Trim(plan, " ()")}
}

// Plan is the rate limit in ~/.claude.json ("Max 20x"), else the bare plan ("Max"); offline.
func (claudeEngine) Plan(_ context.Context, st providers.AuthState) providers.Subscription {
	raw := ClaudeRateTier()
	if m := maxTierRE.FindStringSubmatch(raw); m != nil {
		return providers.Subscription{Raw: raw, Name: "Max " + m[1]}
	}
	return providers.Subscription{Raw: raw, Name: title(st.PlanHint)}
}

func (claudeEngine) Verify(context.Context) (providers.AuthVerdict, string) {
	ok, plan, err := claudeProbe()
	switch {
	case err != nil:
		return providers.VerdictTransient, "claude auth status failed: " + err.Error()
	case ok:
		return providers.VerdictOK, strings.TrimSuffix("logged in "+strings.Trim(plan, " ()"), " ")
	}
	return providers.VerdictNeedsLogin, ""
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

func title(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
