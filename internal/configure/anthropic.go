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

// billingRow is one row of the claude sub-picker: a label/detail pair and what picking it does.
// key, when set, is the provider name this row pins — it is how the picker finds its starting row.
type billingRow struct {
	label, desc, key string
	run              func() bool
}

// otherBackends are the non-anthropic backends Claude Code can run a session on, shown in the claude
// sub-picker below Subscription/API key; each must be a registered provider to get a row.
var otherBackends = []string{"chatgpt", geminiRow, "github-copilot"}

// backendDetail is an otherBackends row's status: whether a working credential is present. chatgpt
// and github-copilot are passthrough providers — CredentialPresent always says yes for those (there is
// no key to check) — so their row asks the vendor's own login state instead, same as keyTargets does.
func backendDetail(reg *providers.Registry, name string) string {
	switch name {
	case "chatgpt":
		return loginDetail(vendors.CodexChatGPTLoginPresent(), "ChatGPT login")
	case "github-copilot":
		return loginDetail(vendors.CopilotLoginPresent(), "GitHub login")
	}
	p, ok := reg.Providers[name]
	if !ok {
		return "not configured"
	}
	if providers.CredentialPresent(p) {
		return "cred:ok  " + p.Kind
	}
	return "cred:missing  " + p.Kind
}

func loginDetail(present bool, what string) string {
	if present {
		return "cred:ok  " + what
	}
	return "cred:missing  " + what
}

// pickClaudeBackend asks which backend the claude agent runs its session on: the two first-party
// billing modes (Subscription, API key), then the other backends the registry has configured
// (chatgpt, gemini, github-copilot), then — once logged in to claude.ai — ways to rotate that
// session (Login, OAuth token). Picking a backend row pins it (keepOrPin/applyProvider), same as
// the plain provider picker; gemini opens its own API-key-vs-Google-login sub-picker.
func pickClaudeBackend(d *dialog, fp *firstParty, cur string) bool {
	loggedIn, plan := fp.login()
	keyOK := fp.key()
	state := func(ok bool, okText, missing string) string {
		if ok {
			return "[" + okText + "]"
		}
		return "[" + missing + "]"
	}
	rows := []billingRow{
		{"Subscription", "OAuth · plan limits · Remote Control " + state(loggedIn, "logged in"+plan, "not logged in"), subscriptionName, func() bool {
			if !loggedIn && confirm(d, "Not logged in to claude.ai — run `claude auth login` now?") {
				if err := runLogin([]string{"claude", "auth", "login"}); err != nil {
					d.printf("anthropic: %v\n", err)
				}
			}
			return keepOrPin(d, cur, subscriptionName)
		}},
		{"API key", "pay per token · paste to set or rotate · no Remote Control " + state(keyOK, "key set", "no key"), anthropicRow, func() bool {
			kt := keyTarget{label: anthropicRow, envVar: anthropicKeyEnv, fixCmd: "claude-code secrets set " + anthropicKeyEnv,
				provider: fp.reg.Providers[anthropicRow]}
			if !ensureKey(d, kt, keyOK) {
				d.printf("anthropic: no usable API key; left on %s\n", billingLabel(cur))
				return false
			}
			return keepOrPin(d, cur, anthropicRow)
		}},
	}
	for _, name := range otherBackends {
		if _, ok := fp.reg.Providers[name]; !ok {
			continue
		}
		n := name
		rows = append(rows, billingRow{n, backendDetail(fp.reg, n), n, func() bool {
			if n == geminiRow {
				return pickGeminiAuth(d, cur)
			}
			return keepOrPin(d, cur, n)
		}})
	}
	if loggedIn {
		rows = append(rows,
			billingRow{"Login", "claude auth login · browser sign-in, refreshes the session", "", func() bool {
				return regenerate(d, keyTarget{label: anthropicRow}, []string{"claude", "auth", "login"}, false)
			}},
			billingRow{"OAuth token", "claude setup-token · mints a new token, rotates " + providers.OAuthTokenEnv, "", func() bool {
				kt := keyTarget{label: anthropicRow, envVar: providers.OAuthTokenEnv}
				return regenerate(d, kt, []string{"claude", "setup-token"}, true)
			}},
		)
	}
	pickRows := make([][2]string, len(rows))
	start := 0
	for i, r := range rows {
		pickRows[i] = [2]string{r.label, r.desc}
		if r.key != "" && r.key == cur {
			start = i
		}
	}
	idx, err := tui.PickBack("claude (current: "+billingLabel(cur)+")", pickRows, start)
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
