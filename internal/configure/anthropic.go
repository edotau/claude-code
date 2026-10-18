package configure

import (
	"fmt"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tui"
)

// The anthropic picker row stands for both first-party billing modes: the subscription OAuth login
// (passthrough provider) and a pay-per-token API key (the anthropic provider).
const (
	anthropicRow     = "anthropic"
	subscriptionName = "subscription"
	anthropicKeyEnv  = "ANTHROPIC_API_KEY"
)

// isAnthropicBilling reports a provider name that is one of the anthropic row's billing modes.
func isAnthropicBilling(name string) bool { return name == anthropicRow || name == subscriptionName }

// pickerNames is the provider list with anthropic first and the subscription provider folded into it.
func pickerNames(reg *providers.Registry) []string {
	var out []string
	if _, ok := reg.Providers[anthropicRow]; ok {
		out = append(out, anthropicRow)
	}
	for _, n := range reg.Names() {
		if !isAnthropicBilling(n) {
			out = append(out, n)
		}
	}
	return out
}

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
		f.loggedIn, f.plan = claudeLoginStatus()
		f.loginDone = true
	}
	return f.loggedIn, f.plan
}

func (f *firstParty) key() bool {
	if !f.keyDone {
		f.keyOK = providers.CredentialPresent(f.reg.Providers[anthropicRow])
		f.keyDone = true
	}
	return f.keyOK
}

// anthropicDetail is the anthropic row's status: the billing mode in use and whether it is valid right now.
func anthropicDetail(fp *firstParty, cur string) string {
	switch cur {
	case subscriptionName:
		if ok, plan := fp.login(); ok {
			return "cred:ok  billing: subscription" + plan
		}
		return "cred:missing  billing: subscription (not logged in)"
	case anthropicRow:
		if fp.key() {
			return "cred:ok  billing: api key"
		}
		return "cred:missing  billing: api key (" + anthropicKeyEnv + " unset)"
	}
	return "subscription or api key"
}

// pickAnthropicBilling asks which first-party billing mode to use, satisfies its credential if absent,
// and pins the matching provider (subscription = OAuth login, anthropic = API key).
func pickAnthropicBilling(d *dialog, fp *firstParty, cur string) bool {
	loggedIn, plan := fp.login()
	keyOK := fp.key()
	state := func(ok bool, okText, missing string) string {
		if ok {
			return "[" + okText + "]"
		}
		return "[" + missing + "]"
	}
	rows := [][2]string{
		{"Subscription", "OAuth · plan limits · Remote Control " + state(loggedIn, "logged in"+plan, "not logged in")},
		{"API key", "pay per token · no Remote Control " + state(keyOK, "key set", "no key")},
	}
	start := 0
	if cur == anthropicRow {
		start = 1
	}
	idx, err := tui.PickBack("anthropic — billing mode (current: "+billingLabel(cur)+")", rows, start)
	if err != nil || idx < 0 {
		d.printf("provider   pinned %s (kept)\n", cur)
		return err == nil
	}
	if idx == 0 {
		if !loggedIn && confirm(d, "Not logged in to claude.ai — run `claude auth login` now?") {
			if err := runLogin([]string{"claude", "auth", "login"}); err != nil {
				d.printf("anthropic: %v\n", err)
			}
		}
		return keepOrPin(d, cur, subscriptionName)
	}
	if !keyOK {
		kt := keyTarget{label: anthropicRow, envVar: anthropicKeyEnv, fixCmd: "claude-code secrets set " + anthropicKeyEnv}
		if !resolveKey(d, kt) {
			d.printf("anthropic: no API key stored; billing left on %s\n", billingLabel(cur))
			return false
		}
	}
	return keepOrPin(d, cur, anthropicRow)
}

// keepOrPin pins name unless it is already the session provider.
func keepOrPin(d *dialog, cur, name string) bool {
	if name == cur {
		d.printf("provider   pinned %s (kept)\n", cur)
		return true
	}
	return applyProvider(d, name)
}

// billingLabel names cur as the picker shows it.
func billingLabel(cur string) string {
	switch cur {
	case subscriptionName:
		return "subscription"
	case anthropicRow:
		return "api key"
	}
	return fmt.Sprintf("%s (not anthropic)", cur)
}
