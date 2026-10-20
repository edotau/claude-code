package configure

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

// The anthropic picker row stands for both first-party billing modes: the subscription OAuth login
// (passthrough provider) and a pay-per-token API key (the anthropic provider).
const (
	anthropicRow     = "anthropic"
	subscriptionName = "subscription"
	anthropicKeyEnv  = "ANTHROPIC_API_KEY"
)

// firstParty memoizes the two anthropic probes for one picker pass — `claude auth status` is a node start,
// CredentialPresent a keyring call — so the row detail and the billing picker pay for each at most once.
type firstParty struct {
	reg                *providers.Registry
	loginDone, keyDone bool
	loggedIn, keyOK    bool
	plan               string
}

func (f *firstParty) login() (loggedIn bool, plan string) {
	if !f.loginDone {
		f.loggedIn, f.plan = providers.ClaudeLoginStatus()
		f.loginDone = true
	}
	return f.loggedIn, f.plan
}

func (f *firstParty) key() bool {
	if !f.keyDone {
		if p := f.reg.Providers[anthropicRow]; p != nil {
			f.keyOK = providers.CredentialPresent(p)
		}
		f.keyDone = true
	}
	return f.keyOK
}

// claudeMenu is the claude agent's submenu: the two first-party billing modes, then Login and OAuth token. Picking a
// mode pins it as the session provider (after [y/N]); other backends stay reachable via `claude-code switch`.
func claudeMenu(d *dialog) agentMenu {
	fp := &firstParty{reg: d.reg}
	cur := currentProvider(d.reg)
	loggedIn, plan := fp.login()
	keyOK := fp.key()
	tier := ""
	if loggedIn {
		tier = claudeTier(plan)
	}
	return agentMenu{claudeState(d, fp, cur, tier), []billingRow{
		{"Subscription", "OAuth · plan limits · Remote Control " + bracket(loggedIn, withTier("logged in", tier), "not logged in"), modeSubscription, func() bool {
			if !loggedIn {
				if !confirm(d, "Not logged in to claude.ai — run `claude auth login` now?") {
					return false
				}
				if err := runLogin([]string{"claude", "auth", "login"}); err != nil {
					d.printf("anthropic: %v\n", err)
					return false
				}
			}
			return keepOrPin(d, cur, subscriptionName)
		}},
		{"API key", "pay per token · paste to set or rotate · no RC " + bracket(keyOK, "key set", "no key"), modeAPIKey, func() bool {
			kt := keyTarget{label: anthropicRow, envVar: anthropicKeyEnv, fixCmd: "claude-code secrets set " + anthropicKeyEnv,
				provider: fp.reg.Providers[anthropicRow]}
			if !ensureKey(d, kt, keyOK) {
				d.printf("anthropic: no usable API key; left on %s\n", billingLabel(cur))
				return false
			}
			return keepOrPin(d, cur, anthropicRow)
		}},
		{"Login", "claude auth login · browser sign-in, refreshes the session", "", func() bool {
			return regenerate(d, keyTarget{label: anthropicRow}, []string{"claude", "auth", "login"}, false)
		}},
		{"OAuth token", "setup-token · mints a token, rotates " + providers.OAuthTokenEnv, "", func() bool {
			kt := keyTarget{label: anthropicRow, envVar: providers.OAuthTokenEnv}
			return regenerate(d, kt, []string{"claude", "setup-token"}, true)
		}},
	}}
}

// claudeState is the pinned backend's mode and check; a non-anthropic pin shows by name.
func claudeState(d *dialog, fp *firstParty, cur, tier string) agentState {
	switch cur {
	case subscriptionName:
		ok, _ := fp.login()
		return agentState{modeSubscription, tier, ok}
	case anthropicRow:
		return agentState{mode: modeAPIKey, verified: fp.key() && keyVerified(d.reg.Providers[anthropicRow])}
	case "chatgpt":
		return agentState{mode: billingLabel(cur), verified: vendors.CodexChatGPTLoginPresent()}
	case "github-copilot":
		return agentState{mode: billingLabel(cur), verified: vendors.CopilotLoginPresent()}
	}
	return agentState{mode: billingLabel(cur), verified: keyVerified(d.reg.Providers[cur])}
}

// keepOrPin pins name unless it is already the session provider.
func keepOrPin(d *dialog, cur, name string) bool {
	if name == cur {
		d.printf("provider   pinned %s (kept)\n", cur)
		return true
	}
	if !switchConfirmed(d, cur, name) {
		d.printf("provider   pinned %s (kept)\n", cur)
		return true
	}
	return applyProvider(d, name)
}

// switchConfirmed asks before a terminal pick re-points every new Claude Code launch (default No): browsing the
// backend list must never switch it silently. Scripted and piped runs keep their explicit pick.
func switchConfirmed(d *dialog, cur, name string) bool {
	if !d.o.Interactive || !isStdinTTY() || d.o.In != os.Stdin {
		return true
	}
	return tui.Confirm("Run every new Claude Code launch on "+name+" instead of "+cur+"?", false)
}

// billingLabel names cur as the picker shows it.
func billingLabel(cur string) string {
	switch cur {
	case subscriptionName:
		return "subscription"
	case anthropicRow:
		return "API key"
	}
	return fmt.Sprintf("%s (not anthropic)", cur)
}
