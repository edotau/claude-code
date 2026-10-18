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
			return "cred:ok  subscription" + plan
		}
		return "cred:missing  subscription (not logged in)"
	case anthropicRow:
		if fp.key() {
			return "cred:ok  API key"
		}
		return "cred:missing  API key (" + anthropicKeyEnv + " unset)"
	}
	return "subscription or api key"
}

// billingRow is one row of the anthropic sub-picker: a label/detail pair and what picking it does.
type billingRow struct {
	label, desc string
	run         func() bool
}

// pickAnthropicBilling asks which first-party billing mode to use, satisfies or rotates its credential,
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
	rows := []billingRow{
		{"Subscription", "OAuth · plan limits · Remote Control " + state(loggedIn, "logged in"+plan, "not logged in"), func() bool {
			if !loggedIn && confirm(d, "Not logged in to claude.ai — run `claude auth login` now?") {
				if err := runLogin([]string{"claude", "auth", "login"}); err != nil {
					d.printf("anthropic: %v\n", err)
				}
			}
			return keepOrPin(d, cur, subscriptionName)
		}},
	}
	if loggedIn {
		rows = append(rows,
			billingRow{"Login", "claude auth login · browser sign-in, refreshes the session", func() bool {
				return regenerate(d, keyTarget{label: anthropicRow}, []string{"claude", "auth", "login"}, false)
			}},
			billingRow{"OAuth token", "claude setup-token · mints a new token, rotates " + providers.OAuthTokenEnv, func() bool {
				kt := keyTarget{label: anthropicRow, envVar: providers.OAuthTokenEnv}
				return regenerate(d, kt, []string{"claude", "setup-token"}, true)
			}},
		)
	}
	rows = append(rows, billingRow{"API key", "pay per token · paste to set, replace or rotate · no Remote Control " + state(keyOK, "key set", "no key"), func() bool {
		kt := keyTarget{label: anthropicRow, envVar: anthropicKeyEnv, fixCmd: "claude-code secrets set " + anthropicKeyEnv,
			provider: fp.reg.Providers[anthropicRow]}
		if !ensureKey(d, kt, keyOK) {
			d.printf("anthropic: no usable API key; left on %s\n", billingLabel(cur))
			return false
		}
		return keepOrPin(d, cur, anthropicRow)
	}})
	pickRows := make([][2]string, len(rows))
	for i, r := range rows {
		pickRows[i] = [2]string{r.label, r.desc}
	}
	start := 0
	if cur == anthropicRow {
		start = len(rows) - 1
	}
	idx, err := tui.PickBack("anthropic (current: "+billingLabel(cur)+")", pickRows, start)
	if err != nil || idx < 0 {
		d.printf("provider   pinned %s (kept)\n", cur)
		return err == nil
	}
	return rows[idx].run()
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
		return "API key"
	}
	return fmt.Sprintf("%s (not anthropic)", cur)
}
