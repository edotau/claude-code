// agentmenu.go — one submenu per agent CLI, all one shape: the two billing modes (Subscription, API key), then the
// actions that refresh or mint the credential (Login, then a token or key mint), then Back.
package configure

import (
	"cmp"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
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

// runAgentMenu shows one agent's submenu with its current mode (and verified) in the title, cursor on that mode's row.
func runAgentMenu(d *dialog, name string, m agentMenu) bool {
	internal := name // the page title carries the display label; undo displayLabel
	if name == displayLabel("gemini") {
		internal = "gemini"
	}
	if r := modelRow(d, internal); r != nil {
		m.rows = append(m.rows, *r)
	}
	for i, r := range m.rows {
		if run := r.run; r.key != "" || r.label == "Login" {
			m.rows[i].run = func() bool { // a successful sign-in switches the agent back on
				ok := run()
				if ok {
					agentActiveOn(d, internal)
				}
				return ok
			}
		}
	}
	m.rows = append(m.rows, agentPinRows(d, internal)...)
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
	if p == nil || !providers.CredentialPresent(p) {
		return false
	}
	status, _, _ := probeCached(keyTarget{provider: p})
	return status == "verified"
}

// keyPresent reports whether provider name has a stored key.
func keyPresent(reg *providers.Registry, provider string) (*providers.Provider, bool) {
	p := profileBy(reg, provider, providers.AuthKey)
	return p, p != nil && providers.CredentialPresent(p)
}

// profileBy is the first profile under provider signing in by method (key, cli, ...), or nil.
func profileBy(reg *providers.Registry, provider, method string) *providers.Provider {
	for _, n := range reg.ProfilesOf(provider) {
		if p := reg.Providers[n]; p.Auth.Method() == method {
			return p
		}
	}
	return nil
}

// profileName is p's name, "" for nil.
func profileName(p *providers.Provider) string {
	if p == nil {
		return ""
	}
	return p.Name
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
		kt := keyTarget{label: label, envVar: p.Auth.Env, fixCmd: "claude-code vault set " + p.Auth.Env, provider: p}
		return ensureKey(d, kt, ok)
	}}
}

// loginOrKey is the state of an agent that bills its own login first, else a BYOK key.
func loginOrKey(d *dialog, loggedIn bool, tier string) agentState {
	if loggedIn {
		return agentState{modeSubscription, tier, true}
	}
	// BYOK fallback (vendors.DescribeRoute order): the anthropic key, handed over as generated config, never `auth login`.
	if p, ok := keyPresent(d.reg, anthropicProvider); ok {
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
		{"Settings", "reasoning effort and verbosity · vault preferences", "", func() bool { return runCodexSettings(d) }},
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
	pinned := providers.Pin(providers.PinCopilotProfile)
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

// copilotEffortChoices: menu label → the --reasoning-effort value copilot's CLI and SDK accept. Fixed
// fallback for when the SDK catalog is unavailable; a reachable catalog drives the row from the model's
// own SupportedReasoningEfforts instead (copilotEffortRow).
var copilotEffortChoices = [][2]string{
	{"Low", "low"}, {"Medium", "medium"}, {"High", "high"}, {"Extra High", "xhigh"}, {"Max", "max"},
}

// copilotEffortLabels maps a catalog reasoning-effort id to its menu label; an unknown id passes through raw.
var copilotEffortLabels = map[string]string{
	"low": "Low", "medium": "Medium", "high": "High", "xhigh": "Extra High", "max": "Max",
}

// copilotContextChoices: menu label → the --context tier value.
var copilotContextChoices = [][2]string{{"200K", "default"}, {"1M", "long_context"}}

// copilotContextBaseline is copilot's default context window (200K); a model above it has a long-context tier.
const copilotContextBaseline = 200_000

// copilotEffortRow pins COPILOT_REASONING_EFFORT; nil when the model doesn't support reasoning effort at
// all. Lists only modelID's own SupportedReasoningEfforts when the SDK catalog resolves it, else falls
// back to the fixed list (today's behavior) since there is nothing to check eligibility against.
func copilotEffortRow(d *dialog, modelID string) *billingRow {
	cur := providers.Pin(providers.PinCopilotEffort)
	m, ok := copilotCatalogModel(d, modelID)
	if !ok {
		desc := "reasoning effort on every launch " + bracket(cur != "", cur, "unset")
		r := billingRow{"Effort", desc, "", func() bool {
			return pickCopilotPin(d, "effort", providers.PinCopilotEffort, copilotEffortChoices, cur)
		}}
		return &r
	}
	if !m.ReasoningEffort || len(m.Efforts) == 0 {
		return nil
	}
	def := "default"
	if m.DefaultEffort != "" {
		def += " (" + m.DefaultEffort + ")"
	}
	desc := "reasoning effort on every launch " + bracket(cur != "", cur, def)
	choices := make([][2]string, len(m.Efforts))
	for i, e := range m.Efforts {
		label := cmp.Or(copilotEffortLabels[e], e)
		if e != "" && e == m.DefaultEffort {
			label += " (default)"
		}
		choices[i] = [2]string{label, e}
	}
	r := billingRow{"Effort", desc, "", func() bool {
		return pickCopilotPin(d, "effort", providers.PinCopilotEffort, choices, cur)
	}}
	return &r
}

// copilotContextEligible: the SDK catalog's MaxContextWindowTokens when it resolves (a model above
// copilot's 200K default has a long-context tier to toggle), else the claude-* prefix heuristic — the
// copilot CLI/SDK expose no other machine-readable "takes --context" flag.
func copilotContextEligible(d *dialog, modelID string) bool {
	if m, ok := copilotCatalogModel(d, modelID); ok {
		return m.ContextTokens > copilotContextBaseline
	}
	return models.FamilyKind(modelID) == models.FamilyClaude
}

// copilotContextRow pins COPILOT_CONTEXT_TIER; nil when modelID has no long-context tier.
func copilotContextRow(d *dialog, modelID string) *billingRow {
	if !copilotContextEligible(d, modelID) {
		return nil
	}
	cur := providers.Pin(providers.PinCopilotContext)
	desc := "context window on every launch " + bracket(cur != "", copilotContextLabel(cur), "auto")
	r := billingRow{"Context", desc, "", func() bool {
		return pickCopilotPin(d, "context", providers.PinCopilotContext, copilotContextChoices, cur)
	}}
	return &r
}

// copilotContextLabel is val's menu label from copilotContextChoices ("200K"/"1M"); val itself if unknown.
func copilotContextLabel(val string) string {
	for _, c := range copilotContextChoices {
		if c[1] == val {
			return c[0]
		}
	}
	return val
}

// clearIneligibleCopilotPins drops the effort/context pins a newly chosen model can't honor, after a
// copilot model switch; each cleared pin prints the reason on one line.
func clearIneligibleCopilotPins(d *dialog, modelID string) {
	if cur := providers.Pin(providers.PinCopilotEffort); cur != "" {
		if m, ok := copilotCatalogModel(d, modelID); ok && !slices.Contains(m.Efforts, cur) {
			clearCopilotPin(d, providers.PinCopilotEffort, cur, modelID, "doesn't support that effort")
		}
	}
	if cur := providers.Pin(providers.PinCopilotContext); cur != "" && !copilotContextEligible(d, modelID) {
		clearCopilotPin(d, providers.PinCopilotContext, cur, modelID, "has no long-context tier")
	}
}

// clearCopilotPin unsets key and reports why.
func clearCopilotPin(d *dialog, key, cur, modelID, why string) {
	if err := providers.SetPins(map[string]string{key: ""}); err != nil {
		d.printf("copilot: %v\n", err)
		return
	}
	d.printf("copilot: cleared %s pin (%s) — %s %s\n", key, cur, modelID, why)
}

// pickCopilotPin lists "unset" plus choices (label, value); a pick pins key to that value (or clears it).
func pickCopilotPin(d *dialog, title, key string, choices [][2]string, cur string) bool {
	return pickVendorPin(d, "copilot", title, key, choices, cur)
}

// pickVendorPin is pickCopilotPin generalized over the vendor naming the pin (its title and printf
// prefix); copilot, codex and gemini's Effort rows all share it.
func pickVendorPin(d *dialog, vendor, title, key string, choices [][2]string, cur string) bool {
	rows := make([][2]string, 0, len(choices)+1)
	rows = append(rows, [2]string{"unset", vendor + "'s own default" + curMark(cur == "")})
	for _, c := range choices {
		rows = append(rows, [2]string{c[0], c[1] + curMark(c[1] == cur)})
	}
	idx, err := pick(d, vendor+" — "+title, rows)
	if err != nil || idx < 0 {
		return false
	}
	val := ""
	if idx > 0 {
		val = choices[idx-1][1]
	}
	if err := providers.SetPins(map[string]string{key: val}); err != nil {
		d.printf("%s: %v\n", vendor, err)
		return false
	}
	d.printf("%s: %s = %s\n", vendor, key, cmp.Or(val, "unset"))
	d.changed = true
	return true
}

// curMark flags the row holding the pin's current value.
func curMark(current bool) string {
	if current {
		return " (current)"
	}
	return ""
}

// copilotKeyRow pastes (or rotates) a GitHub token into COPILOT_GITHUB_TOKEN; it outranks gh's active account.
func copilotKeyRow(d *dialog) billingRow {
	env := vendors.CopilotTokenEnv
	_, src, ok := secrets.Get(env)
	stored := ok && src != "env"
	return billingRow{modeAPIKey, "paste a GitHub token → " + env + " " + bracket(stored, "token set", "no token"), modeAPIKey, func() bool {
		kt := keyTarget{label: "copilot", envVar: env, fixCmd: "claude-code vault set " + env}
		if stored {
			return ensureKey(d, kt, true)
		}
		return runAuthOption(d, kt, authOption{action: "Paste API key", env: env})
	}}
}

// pickGitHubUser lists gh's accounts; a pick is validated (gh has a token GitHub accepts for that user) and pins
// only the name, COPILOT_PROFILE — launches fetch the token with `gh auth token --user`.
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
		return pinCopilotProfile(d, u)
	case idx == n:
		return pinCopilotProfile(d, "")
	default:
		return signIn(d, "gh", []string{"gh", "auth", "login"})
	}
}

// pinCopilotProfile pins the github profile whose login is user (clears the pin when user is ""). With no such
// row it sets login on the personal row when that has none, else asks for a github profile with that login.
func pinCopilotProfile(d *dialog, user string) bool {
	profile := ""
	if user != "" {
		reg, err := providers.Load()
		if err != nil {
			d.printf("copilot: %v\n", err)
			return false
		}
		if profile = providers.GithubAccountName(reg, user); profile == user {
			p := reg.Providers["personal"]
			if p == nil || p.Provider != "github" || p.Login != "" {
				d.printf("copilot: no github profile has login %s; add one with `claude-code vault edit` (providers.github.<name>.login)\n", user)
				return false
			}
			if err := providers.SetProviderFields("personal", map[string]any{"login": user}); err != nil {
				d.printf("copilot: %v\n", err)
				return false
			}
			profile = "personal"
		}
	}
	if err := providers.SetPins(map[string]string{providers.PinCopilotProfile: profile}); err != nil {
		d.printf("copilot: %v\n", err)
		return false
	}
	d.printf("%-10s runs as %s\n", "copilot", bracket(user != "", user+" (pinned as "+profile+")", "gh's active account"))
	return true
}

// opencodeMenu is the home row's state only; its page is runOpencodePage (opencode_page.go).
func opencodeMenu(d *dialog) agentMenu {
	if vendors.OpencodeOwnLoginPresent() || vendors.OpencodeCopilotLoginPresent() {
		return agentMenu{state: agentState{modeSubscription, "", true}}
	}
	return agentMenu{state: agentState{mode: modeNone}}
}
