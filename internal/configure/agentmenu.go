// agentmenu.go — one submenu per agent CLI, all one shape: the two billing modes (Subscription, API key), then the
// actions that refresh or mint the credential (Login, then a token or key mint), then Back.
package configure

import (
	"os"
	"os/exec"
	"strings"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

const (
	modeSubscription = "subscription"
	modeAPIKey       = "API key"
	modeNone         = "not set up"
)

// agentState is how an agent bills today and whether that credential checked out.
type agentState struct {
	mode     string // modeSubscription, modeAPIKey, modeNone, or a non-anthropic backend's label
	tier     string // plan name when the sign-in reports one ("Max 20x", "Plus", "Pro+")
	verified bool
}

// label is the top menu's mode column: "subscription (Max 20x)", "API key" or "not set up".
func (s agentState) label() string {
	if s.mode == modeSubscription {
		return withTier(s.mode, s.tier)
	}
	return s.mode
}

func (s agentState) check() string {
	if s.verified {
		return "verified"
	}
	return "not verified"
}

// billingRow is one submenu row: label, detail, the mode it stands for ("" for an action row), and its action.
type billingRow struct {
	label, desc, key string
	run              func() bool
}

type agentMenu struct {
	state agentState
	rows  []billingRow
}

// agentMenus builds each agent's submenu; the top menu reads only its state.
var agentMenus = map[string]func(d *dialog) agentMenu{
	"claude": claudeMenu, "codex": codexMenu, "gemini": geminiMenu, "opencode": opencodeMenu, "copilot": copilotMenu,
}

// pickAgent is the submenu picker seam; tests replace it.
var pickAgent = tui.PickBack

// displayLabel is the user-facing name for an agent row: every vendor is its own name except gemini,
// whose runner is agy 1.4.2 (Antigravity) — the internal vendor/runner name stays "gemini" everywhere else.
func displayLabel(name string) string {
	if name == "gemini" {
		return "antigravity"
	}
	return name
}

// internalName undoes displayLabel: the agent page title carries the display label.
func internalName(label string) string {
	if label == displayLabel("gemini") {
		return "gemini"
	}
	return label
}

// runAgentMenu shows one agent's submenu with its current mode (and verified) in the title, cursor on that mode's row.
func runAgentMenu(d *dialog, name string, m agentMenu) bool {
	if r := modelRow(d, internalName(name)); r != nil {
		m.rows = append(m.rows, *r)
	}
	title := name + " (current: " + m.state.mode
	if m.state.verified {
		title += " · verified"
	}
	title += ")"
	rows := make([][2]string, len(m.rows))
	start := 0
	for i, r := range m.rows {
		rows[i] = [2]string{r.label, r.desc}
		if r.key != "" && r.key == m.state.mode {
			start = i
		}
	}
	idx, err := pickAgent(title, rows, start)
	if err != nil || idx < 0 {
		return false
	}
	return m.rows[idx].run()
}

// bracket is a row's trailing state: [yes] or [no].
func bracket(ok bool, yes, no string) string {
	if ok {
		return "[" + yes + "]"
	}
	return "[" + no + "]"
}

// keyVerified live-probes a keyed provider; false when absent or not accepted.
func keyVerified(p *providers.Provider) bool {
	return p != nil && providers.CredentialPresent(p) && probeStatus(p) == "verified"
}

// keyPresent reports whether provider name has a stored key.
func keyPresent(reg *providers.Registry, name string) (*providers.Provider, bool) {
	p, ok := reg.Providers[name]
	return p, ok && providers.CredentialPresent(p)
}

// signIn runs a vendor's own sign-in CLI after the [y/N] confirm and reports the outcome.
func signIn(d *dialog, label string, argv []string) bool {
	if err := runLogin(argv); err != nil {
		d.printf("%s: %v\n", label, err)
		return false
	}
	d.printf("%-10s signed in\n", label)
	return true
}

// keyRow is the API key row shared by every agent: ensure (paste, probe, store) the provider's key.
func keyRow(d *dialog, label, provider, desc string) billingRow {
	p, ok := keyPresent(d.reg, provider)
	return billingRow{modeAPIKey, desc + " " + bracket(ok, "key set", "no key"), modeAPIKey, func() bool {
		if p == nil {
			d.printf("%s: no %s provider registered\n", label, provider)
			return false
		}
		kt := keyTarget{label: label, envVar: p.Auth.Env, fixCmd: "claude-code secrets set " + p.Auth.Env, provider: p}
		return ensureKey(d, kt, ok)
	}}
}

// byokKey is the key copilot and opencode fall back to without their login (vendors.DescribeRoute order): the
// anthropic key; the harness hands it over as generated config, never `auth login`.
func byokKey(reg *providers.Registry) (*providers.Provider, string) {
	for _, name := range []string{anthropicRow} {
		if p, ok := keyPresent(reg, name); ok {
			return p, p.Auth.Env
		}
	}
	return nil, ""
}

// byokRow is copilot's and opencode's API key row: shows which fallback key resolves, sets the anthropic one.
func byokRow(d *dialog, label string) billingRow {
	r := keyRow(d, label, anthropicRow, "pay per token via a stored key")
	if _, env := byokKey(d.reg); env != "" {
		r.desc = strings.TrimSuffix(r.desc, "[no key]") + "[" + env + " set]"
	}
	return r
}

// loginOrKey is the state of an agent that bills its own login first, else a BYOK key.
func loginOrKey(d *dialog, loggedIn bool, tier string) agentState {
	if loggedIn {
		return agentState{modeSubscription, tier, true}
	}
	if p, _ := byokKey(d.reg); p != nil {
		return agentState{mode: modeAPIKey, verified: keyVerified(p)}
	}
	return agentState{mode: modeNone}
}

func codexMenu(d *dialog) agentMenu {
	loggedIn := vendors.CodexChatGPTLoginPresent()
	tier := ""
	if loggedIn {
		tier = codexTier()
	}
	st := agentState{mode: modeNone}
	if loggedIn {
		st = agentState{modeSubscription, tier, true}
	} else if kt := vendorKeyTarget(d.reg, "codex"); kt.present {
		st = agentState{mode: modeAPIKey, verified: kt.provider == nil || keyVerified(kt.provider)}
	}
	key := keyRow(d, "codex", "openai", "pay per token · paste to set or rotate OPENAI_API_KEY")
	ensure := key.run
	key.run = func() bool {
		if !ensure() {
			return false
		}
		if loggedIn && actionConfirmed("Switch codex from its ChatGPT login to OPENAI_API_KEY (`codex login --with-api-key`)") {
			return codexUseKey(d)
		}
		return true
	}
	return agentMenu{st, []billingRow{
		{"Subscription", "ChatGPT login · plan limits " + bracket(loggedIn, withTier("logged in", tier), "not logged in"), modeSubscription, func() bool {
			if loggedIn {
				d.printf("%-10s on its ChatGPT login (kept)\n", "codex")
				return true
			}
			return signIn(d, "codex", []string{"codex", "login"})
		}},
		key,
		{"Login", "codex login · browser sign-in, refreshes the session", "", func() bool {
			return signIn(d, "codex", []string{"codex", "login"})
		}},
	}}
}

// codexKeyLogin feeds key to `codex login --with-api-key` on stdin; tests replace it.
var codexKeyLogin = func(key string) error {
	cmd := exec.Command("codex", "login", "--with-api-key")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(key), os.Stdout, os.Stderr
	return cmd.Run()
}

// codexUseKey points codex's own auth at the stored OPENAI_API_KEY.
func codexUseKey(d *dialog) bool {
	key, _, ok := secrets.Get("OPENAI_API_KEY")
	if !ok || key == "" {
		d.printf("codex: OPENAI_API_KEY not found in a stored plane\n")
		return false
	}
	if err := codexKeyLogin(key); err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	d.printf("%-10s now bills OPENAI_API_KEY per token\n", "codex")
	return true
}

func copilotMenu(d *dialog) agentMenu {
	auth, loggedIn := vendors.ResolveCopilotAuth()
	tier := ""
	if loggedIn {
		tier = copilotTier()
	}
	st := loginOrKey(d, loggedIn, tier)
	_, _, tokenSet := secrets.Get(vendors.CopilotTokenEnv)
	rows := []billingRow{
		{"Subscription", "GitHub Copilot · " + bracket(loggedIn, withTier(auth.Label(), tier), "not logged in"), modeSubscription, func() bool {
			if loggedIn {
				d.printf("%-10s runs as %s (kept)\n", "copilot", auth.Label())
				return true
			}
			return signIn(d, "copilot", []string{"copilot", "login"})
		}},
		byokRow(d, "copilot"),
		{"Login", "copilot login · browser device sign-in, refreshes the session", "", func() bool {
			return signIn(d, "copilot", []string{"copilot", "login"})
		}},
		{"GitHub account", "pick the account copilot runs as " + bracket(tokenSet, "pinned", "copilot login"), "", func() bool {
			return pickCopilotAccount(d, auth)
		}},
	}
	return agentMenu{st, rows}
}

// pickCopilotAccount lists every GitHub account on this box; a gh account pins its token as COPILOT_GITHUB_TOKEN
// in the secrets manager, "copilot login" clears the pin, and the last two rows add a token or a new sign-in.
func pickCopilotAccount(d *dialog, cur vendors.CopilotAuth) bool {
	if !isStdinTTY() || d.o.In != os.Stdin {
		d.printf("copilot: runs as %s; accounts: %s\n", cur.Label(), strings.Join(vendors.GHAccounts(), ", "))
		return false
	}
	gh := vendors.GHAccounts()
	active := vendors.GHActiveAccount()
	var rows [][2]string
	for _, u := range gh {
		desc := "personal"
		if strings.Contains(u, "_") { // only enterprise managed users carry the _shortcode suffix
			desc = "enterprise"
		}
		if u == active {
			desc += " · gh active"
		}
		if u == cur.Account {
			desc += " · in use"
		}
		rows = append(rows, [2]string{u, desc})
	}
	stored := vendors.CopilotStoredLogin()
	rows = append(rows, [2]string{"copilot login", "unpin; use copilot's own sign-in " + bracket(stored != "", stored, "none")},
		[2]string{"Paste token", "a GitHub OAuth token (gho_/ghp_/github_pat_)"},
		[2]string{"New sign-in", "gh auth login · adds another account to this list"})
	idx, err := tui.PickBack("copilot — which GitHub account? (now: "+cur.Label()+")", rows, 0)
	if err != nil || idx < 0 {
		return false
	}
	switch n := len(gh); {
	case idx < n:
		tok := vendors.GHToken(gh[idx])
		if tok == "" || vendors.GitHubLogin(tok) != gh[idx] {
			d.printf("copilot: gh has no working token for %s; run gh auth login\n", gh[idx])
			return false
		}
		d.printf("%-10s pinned to %s (%s in the secrets manager)\n", "copilot", gh[idx], vendors.CopilotTokenEnv)
		return storeValue(d, vendors.CopilotTokenEnv, tok, false)
	case idx == n:
		if err := secrets.Delete(vendors.CopilotTokenEnv); err != nil {
			d.printf("copilot: %v\n", err)
			return false
		}
		d.printf("%-10s unpinned; runs on copilot's own sign-in %s\n", "copilot", bracket(stored != "", stored, "(none yet)"))
		return true
	case idx == n+1:
		return setCopilotToken(d)
	default:
		return signIn(d, "gh", []string{"gh", "auth", "login"})
	}
}

// setCopilotToken stores a pasted GitHub OAuth token as COPILOT_GITHUB_TOKEN after naming the account it belongs to.
func setCopilotToken(d *dialog) bool {
	val, err := secretLine(d, "Paste a GitHub OAuth token (gho_/ghp_/github_pat_; input hidden): ")
	if err != nil || strings.TrimSpace(val) == "" {
		d.println("copilot: no token entered; nothing changed")
		return false
	}
	val = strings.TrimSpace(val)
	login := vendors.GitHubLogin(val)
	if login == "" {
		d.println("copilot: GitHub rejected that token (api.github.com/user); nothing stored")
		return false
	}
	d.printf("%-10s token belongs to %s; copilot launches will run as it\n", "copilot", login)
	return storeValue(d, vendors.CopilotTokenEnv, val, false)
}

func opencodeMenu(d *dialog) agentMenu {
	loggedIn := vendors.OpencodeCopilotLoginPresent()
	return agentMenu{loginOrKey(d, loggedIn, ""), []billingRow{
		{"Subscription", "GitHub Copilot via opencode · plan limits " + bracket(loggedIn, "logged in", "not logged in"), modeSubscription, func() bool {
			if loggedIn {
				d.printf("%-10s on its GitHub Copilot login (kept)\n", "opencode")
				return true
			}
			return signIn(d, "opencode", []string{"opencode", "auth", "login"})
		}},
		byokRow(d, "opencode"),
		{"Login", "opencode auth login · browser sign-in, refreshes the session", "", func() bool {
			return signIn(d, "opencode", []string{"opencode", "auth", "login"})
		}},
	}}
}
