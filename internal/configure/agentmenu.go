// agentmenu.go — one submenu per agent CLI, all one shape: the two billing modes (Subscription, API key), then the
// actions that refresh or mint the credential (Login, then a token or key mint), then Back.
package configure

import (
	"os"
	"os/exec"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
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

// byokRow is opencode's API key row: shows which fallback key resolves, sets the anthropic one.
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
	bin, err := paths.VendorBin("codex")
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "login", "--with-api-key")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(key), os.Stdout, os.Stderr
	return cmd.Run()
}

// codexUseKey points codex's own auth at the stored OPENAI_API_KEY.
func codexUseKey(d *dialog) bool {
	key, _, ok := secrets.Get("OPENAI_API_KEY")
	if !ok || key == "" {
		d.printf("codex: OPENAI_API_KEY not found in the vault\n")
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
	if auth.Source == vendors.SrcStoredToken {
		st = agentState{mode: modeAPIKey, verified: auth.Account != ""}
	}
	pinned := providers.Pin(providers.PinGitHubUser)
	rows := []billingRow{
		{"Subscription", "GitHub Copilot · " + bracket(loggedIn, withTier(auth.Label(), tier), "not logged in"), modeSubscription, func() bool {
			if loggedIn {
				d.printf("%-10s runs as %s (kept)\n", "copilot", auth.Label())
				return true
			}
			return signIn(d, "copilot", []string{"copilot", "login"})
		}},
		copilotKeyRow(d),
		{"Login", "copilot login · browser device sign-in, refreshes the session", "", func() bool {
			return signIn(d, "copilot", []string{"copilot", "login"})
		}},
		{"User", "the gh account copilot runs as " + bracket(pinned != "", pinned, "gh active"), "", func() bool {
			return pickGitHubUser(d, auth)
		}},
	}
	return agentMenu{st, rows}
}

// copilotKeyRow pastes (or rotates) a GitHub token into COPILOT_GITHUB_TOKEN; it outranks gh's active account.
func copilotKeyRow(d *dialog) billingRow {
	env := vendors.CopilotTokenEnv
	_, src, ok := secrets.Get(env)
	stored := ok && src != "env"
	return billingRow{modeAPIKey, "paste a GitHub token → " + env + " " + bracket(stored, "token set", "no token"), modeAPIKey, func() bool {
		kt := keyTarget{label: "copilot", envVar: env, fixCmd: "claude-code secrets set " + env}
		if stored {
			return ensureKey(d, kt, true)
		}
		return runAuthOption(d, kt, authOption{action: "Paste API key", env: env})
	}}
}

// pickGitHubUser lists gh's accounts; a pick is validated (gh has a token GitHub accepts for that user) and pins
// only the name, COPILOT_GITHUB_USER — launches fetch the token with `gh auth token --user`.
func pickGitHubUser(d *dialog, cur vendors.CopilotAuth) bool {
	gh := vendors.GHAccounts()
	if !isStdinTTY() || d.o.In != os.Stdin {
		d.printf("copilot: runs as %s; gh accounts: %s\n", cur.Label(), strings.Join(gh, ", "))
		return false
	}
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
	rows = append(rows, [2]string{"gh active", "unpin; follow gh's active account " + bracket(active != "", active, "none")},
		[2]string{"New sign-in", "gh auth login · adds another account to this list"})
	idx, err := tui.PickBack("copilot — which GitHub user? (now: "+cur.Label()+")", rows, 0)
	if err != nil || idx < 0 {
		return false
	}
	switch n := len(gh); {
	case idx < n:
		u := gh[idx]
		if tok := vendors.GHToken(u); tok == "" || vendors.GitHubLogin(tok) != u {
			d.printf("copilot: gh has no working token for %s; run gh auth login\n", u)
			return false
		}
		return pinGitHubUser(d, u)
	case idx == n:
		return pinGitHubUser(d, "")
	default:
		return signIn(d, "gh", []string{"gh", "auth", "login"})
	}
}

// pinGitHubUser writes (or clears, user "") COPILOT_GITHUB_USER in the vault pins.
func pinGitHubUser(d *dialog, user string) bool {
	if err := providers.SetPins(map[string]string{providers.PinGitHubUser: user}); err != nil {
		d.printf("copilot: %v\n", err)
		return false
	}
	d.printf("%-10s runs as %s\n", "copilot", bracket(user != "", user+" (pinned)", "gh's active account"))
	return true
}

// opencodeMenu is the home row's state only; its page is runOpencodePage (opencode_page.go).
func opencodeMenu(d *dialog) agentMenu {
	return agentMenu{state: loginOrKey(d, vendors.OpencodeCopilotLoginPresent(), "")}
}
