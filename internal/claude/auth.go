// auth.go — claude's agent.Authenticator: the claude.ai login read from `claude auth status` and ~/.claude.json.
package claude

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

var _ agent.Authenticator = claudeAgent

// AuthStatus is the claude.ai login from `claude auth status`; a failed probe sets Err (unknown, not logged out).
func (claudeEngine) AuthStatus() providers.AuthState {
	ok, plan, err := claudeLoginProbe()
	if err != nil {
		return providers.AuthState{Err: err}
	}
	if !ok {
		return providers.AuthState{}
	}
	return providers.AuthState{Present: true, Source: "claude.ai login", Method: providers.AuthCLI, PlanHint: strings.Trim(plan, " ()")}
}

// Plan is the plan name ~/.claude.json shows ("Ultra"), else the rate limit ("Ultra", "Max 5x"), else the bare plan; offline.
func (claudeEngine) Plan(_ context.Context, st providers.AuthState) providers.Subscription {
	acct := providers.ClaudeAccount()
	raw := acct.Tier()
	if acct.PlanName != "" {
		return providers.Subscription{Raw: raw, Name: acct.PlanName}
	}
	if name := providers.MaxTierName(raw); name != "" {
		return providers.Subscription{Raw: raw, Name: name}
	}
	return providers.Subscription{Raw: raw, Name: textutil.Title(st.PlanHint)}
}

func (claudeEngine) Verify(context.Context) (providers.AuthVerdict, string) {
	ok, plan, err := claudeLoginProbe()
	switch {
	case err != nil:
		return providers.VerdictTransient, "claude auth status failed: " + err.Error()
	case ok:
		return providers.VerdictOK, strings.TrimSuffix("logged in "+strings.Trim(plan, " ()"), " ")
	}
	return providers.VerdictNeedsLogin, ""
}

// claudeProbeTTL is how long a logged-in `claude auth status` result is reused.
const claudeProbeTTL = 10 * time.Second

// claudeProbe is one `claude auth status` answer.
type claudeProbe struct {
	loggedIn bool
	plan     string
}

// claudeProbeMemo is single-flight; only a logged-in answer is kept, so the recheck after a login sees it.
var claudeProbeMemo paths.Memo[struct{}, claudeProbe]

// claudeLoginProbe asks `claude auth status` (JSON); err != nil means unknown. Single-flight; logged-in memo 10s.
func claudeLoginProbe() (loggedIn bool, plan string, err error) {
	p, err := claudeProbeMemo.Do(struct{}{}, func(p claudeProbe, err error) time.Duration {
		if err == nil && p.loggedIn {
			return claudeProbeTTL
		}
		return -1
	}, func() (claudeProbe, error) {
		in, plan, err := runClaudeProbe()
		return claudeProbe{in, plan}, err
	})
	return p.loggedIn, p.plan, err
}

// runClaudeProbe spawns `claude auth status` (≤8s) and parses its JSON.
func runClaudeProbe() (loggedIn bool, plan string, err error) {
	bin, err := ClaudeBinary()
	if err != nil {
		return false, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	out, runErr := agent.ProbeCLI(ctx, bin, agent.EnvSpec{Mode: agent.EnvAgent}, false, "auth", "status")
	var st struct {
		LoggedIn         bool   `json:"loggedIn"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil { // logged-out may exit nonzero but still print JSON
		if runErr != nil {
			return false, "", runErr
		}
		return false, "", jerr
	}
	if st.SubscriptionType != "" {
		plan = " (" + st.SubscriptionType + ")"
	}
	return st.LoggedIn, plan, nil
}
