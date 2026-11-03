// auth.go — claude's agent.Authenticator: the claude.ai login read from `claude auth status` and ~/.claude.json.
package claude

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

var _ agent.Authenticator = claudeAgent

// claudeProbe is `claude auth status`; a seam so tests stay off the real CLI.
var claudeProbe = ClaudeLoginProbe

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
	ok, plan, err := claudeProbe()
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

// probeCall is one in-flight `claude auth status` run that concurrent callers wait on.
type probeCall struct {
	done     chan struct{}
	loggedIn bool
	plan     string
	err      error
}

// claudeProbeMemo holds the last logged-in probe and the run in flight; logged-out and errors are never stored.
var claudeProbeMemo struct {
	mu       sync.Mutex
	at       time.Time
	loggedIn bool
	plan     string
	flight   *probeCall
}

// resetClaudeProbe drops the memo so the next ClaudeLoginProbe runs the binary again.
func resetClaudeProbe() {
	claudeProbeMemo.mu.Lock()
	defer claudeProbeMemo.mu.Unlock()
	claudeProbeMemo.at = time.Time{}
}

// ClaudeLoginProbe asks `claude auth status` (JSON); err != nil means unknown. Single-flight; logged-in memo 10s.
func ClaudeLoginProbe() (loggedIn bool, plan string, err error) {
	m := &claudeProbeMemo
	m.mu.Lock()
	if !m.at.IsZero() && time.Since(m.at) < claudeProbeTTL {
		defer m.mu.Unlock()
		return m.loggedIn, m.plan, nil
	}
	if c := m.flight; c != nil {
		m.mu.Unlock()
		<-c.done
		return c.loggedIn, c.plan, c.err
	}
	c := &probeCall{done: make(chan struct{})}
	m.flight = c
	m.mu.Unlock()

	c.loggedIn, c.plan, c.err = runClaudeProbe()

	m.mu.Lock()
	if c.err == nil && c.loggedIn { // a logged-out result is not reused, so the recheck after a login sees it
		m.at, m.loggedIn, m.plan = time.Now(), c.loggedIn, c.plan
	}
	m.flight = nil
	m.mu.Unlock()
	close(c.done)
	return c.loggedIn, c.plan, c.err
}

// runClaudeProbe spawns `claude auth status` (≤8s) and parses its JSON.
func runClaudeProbe() (loggedIn bool, plan string, err error) {
	bin, err := paths.AgentBin("claude")
	if err != nil {
		return false, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, runErr := exec.CommandContext(ctx, bin, "auth", "status").Output()
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
