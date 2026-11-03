package menu

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/codex"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secretstore"
	"github.com/edotau/claude-code/internal/ui/ansi"
	uidialog "github.com/edotau/claude-code/internal/ui/dialog"
	"github.com/edotau/claude-code/internal/vault"
)

const (
	modeSubscription = "subscription"

	modeAPIKey = "API key"

	modeNone = "not set up"
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
		return configure.WithTier(s.mode, s.tier)
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
var pickAgent = func(d *dialog, title string, rows [][2]string, start int) (int, error) {
	if !d.shell().TTY { // the numbered prompt prints rows raw: no span markers
		rows = slices.Clone(rows)
		for i := range rows {
			rows[i][1] = ansi.Strip(rows[i][1])
		}
	}
	return d.shell().Pick(title, rows, start)
}

// displayLabel is the user-facing name for an agent row: its roster DisplayName, else its own name.
func displayLabel(name string) string {
	if s, ok := agent.LookupSchema(name); ok && s.CLI.DisplayName != "" {
		return s.CLI.DisplayName
	}
	return name
}

// runAgentMenu shows one agent's submenu with its current mode (and verified) in the title, cursor on that mode's row.
func runAgentMenu(d *dialog, name string, m agentMenu) bool {
	internal := name // the page title carries the display label; undo displayLabel
	for _, s := range agent.Roster() {
		if s.CLI.DisplayName == name {
			internal = s.Name
		}
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
	if _, ok := setupLogins[internal]; ok {
		m.rows = append([]billingRow{setupRow(d, internal)}, m.rows...)
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
	idx, err := pickAgent(d, title, rows, start)
	if err != nil || idx < 0 {
		return false
	}
	return m.rows[idx].run()
}

// signIn runs an agent's own sign-in CLI after the [y/N] confirm and reports the outcome.
func signIn(d *dialog, label string, argv []string) bool {
	if err := configure.RunLogin(argv); err != nil {
		d.printf("%s: %v\n", label, err)
		return false
	}
	d.printf("%-10s signed in\n", label)
	return true
}

// keyRow is the API key row shared by every agent: ensure (paste, probe, store) the provider's key.
func keyRow(d *dialog, label, provider, desc string) billingRow {
	p, ok := configure.KeyPresent(d.reg, provider)
	return billingRow{modeAPIKey, desc + " " + Bracket(ok, "key set", "no key"), modeAPIKey, func() bool {
		if p == nil {
			d.printf("%s: no %s provider registered\n", label, provider)
			return false
		}
		kt := configure.KeyTarget{Label: label, EnvVar: p.Auth.Env, FixCmd: "claude-code vault set " + p.Auth.Env, Provider: p}
		return ensureKey(d, kt, ok)
	}}
}

// loginOrKey is the state of an agent that bills its own login first, else a BYOK key.
func loginOrKey(d *dialog, loggedIn bool, tier string) agentState {
	if loggedIn {
		return agentState{modeSubscription, tier, true}
	}
	// BYOK fallback (agent.DescribeRoute order): the anthropic key, handed over as generated config, never `auth login`.
	if p, ok := configure.KeyPresent(d.reg, anthropicProvider); ok {
		return agentState{mode: modeAPIKey, verified: configure.KeyVerified(p)}
	}
	return agentState{mode: modeNone}
}

func codexMenu(d *dialog) agentMenu {
	loggedIn := codex.CodexChatGPTLoginPresent()
	tier := ""
	if loggedIn {
		tier = configure.CodexTier()
	}
	st := agentState{mode: modeNone}
	if loggedIn {
		st = agentState{modeSubscription, tier, true}
	} else if kt := configure.AgentKeyTarget(d.reg, "codex"); kt.Present {
		st = agentState{mode: modeAPIKey, verified: kt.Provider == nil || configure.KeyVerified(kt.Provider)}
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
	pinned := vault.GetAgentRecord("codex").Profile
	return agentMenu{st, []billingRow{
		{"Subscription", "ChatGPT login · plan limits " + Bracket(loggedIn, configure.WithTier("logged in", tier), "not logged in"), modeSubscription, func() bool {
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
		{"Models", "discover what " + cmp.Or(pinned, "the pinned profile") + " serves", "", func() bool { return codexModels(d, pinned) }},
	}}
}

// codexModels previews the menu shared by opencode, copilot and codex merged with what profile serves, writes it only
// after a confirm, then lists the ids codex can use over Responses.
func codexModels(d *dialog, profile string) bool {
	if profile == "" {
		d.printf("codex: no profile pinned; set one on the Profile row\n")
		return false
	}
	pl, err := opencode.MenuSync(profile, opencode.SyncOptions{})
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	d.printf("codex: %s serves %d chat models, %d not in the shared menu\n", profile, len(pl.IDs), len(pl.Changes))
	for _, c := range pl.Changes {
		d.printf("+ %s  responses=%v\n", c.Model.ID, c.Model.Responses)
	}
	menuNow, applied := pl.Menu, false
	if len(pl.Changes) > 0 && actionConfirmed(fmt.Sprintf("Add %d model(s) to the menu shared with opencode and copilot", len(pl.Changes))) {
		_, err := pl.Seal()
		if err == nil {
			err = providers.SetOpencodeMenu(pl.Next)
		}
		if err != nil {
			d.printf("codex: %v\n", err)
			return false
		}
		menuNow, applied = pl.Next, true
		d.printf("codex: menu updated; relaunch codex to pick it up\n")
	}
	var usable []string
	for _, m := range menuNow {
		if m.Responses && slices.Contains(pl.IDs, m.ID) {
			usable = append(usable, m.ID)
		}
	}
	d.printf("codex: usable over Responses on %s: %s\n", profile, cmp.Or(strings.Join(usable, ", "), "none"))
	return applied
}

// codexKeyLogin feeds key to `codex login --with-api-key` on stdin; tests replace it.
var codexKeyLogin = func(key string) error {
	bin, err := paths.AgentBin("codex")
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "login", "--with-api-key")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(key), os.Stdout, os.Stderr
	return cmd.Run()
}

// codexUseKey points codex's own auth at the stored OPENAI_API_KEY.
func codexUseKey(d *dialog) bool {
	key, _, ok := secretstore.Get("OPENAI_API_KEY")
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
	auth, loggedIn := copilot.ResolveCopilotAuth()
	tier := ""
	if loggedIn {
		tier = configure.CopilotTier()
	}
	st := loginOrKey(d, loggedIn, tier)
	if auth.Source == copilot.SrcStoredToken {
		st = agentState{mode: modeAPIKey, verified: auth.Account != ""}
	}
	pinned := vault.GetAgentRecord("copilot").Profile
	rows := []billingRow{
		{"Subscription", "GitHub Copilot " + Bracket(loggedIn, configure.WithTier(auth.Label(), tier), "not logged in"), modeSubscription, func() bool {
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
		{"User", "the gh account copilot runs as " + Bracket(pinned != "", pinned, "gh active"), "", func() bool {
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
	cur := vault.GetAgentRecord("copilot").Effort
	m, ok := copilotCatalogModel(d, modelID)
	if !ok {
		desc := "reasoning effort on every launch " + Bracket(cur != "", cur, "unset")
		r := billingRow{"Effort", desc, "", func() bool {
			return pickCopilotPin(d, "effort", "EFFORT", copilotEffortChoices, cur)
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
	desc := "reasoning effort on every launch " + Bracket(cur != "", cur, def)
	choices := make([][2]string, len(m.Efforts))
	for i, e := range m.Efforts {
		label := cmp.Or(copilotEffortLabels[e], e)
		if e != "" && e == m.DefaultEffort {
			label += " (default)"
		}
		choices[i] = [2]string{label, e}
	}
	r := billingRow{"Effort", desc, "", func() bool {
		return pickCopilotPin(d, "effort", "EFFORT", choices, cur)
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
	cur := vault.GetAgentRecord("copilot").Context
	desc := "context window on every launch " + Bracket(cur != "", copilotContextLabel(cur), "auto")
	r := billingRow{"Context", desc, "", func() bool {
		return pickCopilotPin(d, "context", "CONTEXT", copilotContextChoices, cur)
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
	rec := vault.GetAgentRecord("copilot")
	if cur := rec.Effort; cur != "" {
		if m, ok := copilotCatalogModel(d, modelID); ok && !slices.Contains(m.Efforts, cur) {
			clearCopilotPin(d, "EFFORT", cur, modelID, "doesn't support that effort")
		}
	}
	if cur := rec.Context; cur != "" && !copilotContextEligible(d, modelID) {
		clearCopilotPin(d, "CONTEXT", cur, modelID, "has no long-context tier")
	}
}

// clearCopilotPin unsets the record field key and reports why.
func clearCopilotPin(d *dialog, key, cur, modelID, why string) {
	if _, err := writeAgentField(d, "copilot", key, ""); err != nil {
		d.printf("copilot: %v\n", err)
		return
	}
	d.printf("copilot: cleared %s pin (%s) — %s %s\n", strings.ToLower(key), cur, modelID, why)
}

// pickCopilotPin lists "unset" plus choices (label, value); a pick pins key to that value (or clears it).
func pickCopilotPin(d *dialog, title, key string, choices [][2]string, cur string) bool {
	rows := make([][2]string, 0, len(choices)+1)
	rows = append(rows, [2]string{"unset", "copilot's own default" + uidialog.Mark(cur == "")})
	for _, c := range choices {
		rows = append(rows, [2]string{c[0], c[1] + uidialog.Mark(c[1] == cur)})
	}
	idx, err := d.shell().Pick("copilot — "+title, rows)
	if err != nil || idx < 0 {
		return false
	}
	val := ""
	if idx > 0 {
		val = choices[idx-1][1]
	}
	if _, err := writeAgentField(d, "copilot", key, val); err != nil {
		d.printf("copilot: %v\n", err)
		return false
	}
	return true
}

// copilotKeyRow pastes (or rotates) a GitHub token into COPILOT_GITHUB_TOKEN; it outranks gh's active account.
func copilotKeyRow(d *dialog) billingRow {
	env := copilot.CopilotTokenEnv
	_, src, ok := secretstore.Get(env)
	stored := ok && src != "env"
	return billingRow{modeAPIKey, "paste a GitHub token → " + env + " " + Bracket(stored, "token set", "no token"), modeAPIKey, func() bool {
		kt := configure.KeyTarget{Label: "copilot", EnvVar: env, FixCmd: "claude-code vault set " + env}
		if stored {
			return ensureKey(d, kt, true)
		}
		return runAuthOption(d, kt, configure.AuthOption{Action: "Paste API key", Env: env})
	}}
}

// pickGitHubUser lists gh's accounts; a pick is validated (gh has a token GitHub accepts for that user) and pins
// only the name, COPILOT_PROFILE — launches fetch the token with `gh auth token --user`.
func pickGitHubUser(d *dialog, cur copilot.CopilotAuth) bool {
	gh := copilot.GHAccounts()
	if !isStdinTTY() || d.o.In != os.Stdin {
		d.printf("copilot: runs as %s; gh accounts: %s\n", cur.Label(), strings.Join(gh, ", "))
		return false
	}
	active := copilot.GHActiveAccount()
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
	rows = append(rows, [2]string{"gh active", "unpin; follow gh's active account " + Bracket(active != "", active, "none")},
		[2]string{"New sign-in", "gh auth login · adds another account to this list"})
	idx, err := d.shell().Pick("copilot — which GitHub user? (now: "+cur.Label()+")", rows, 0)
	if err != nil || idx < 0 {
		return false
	}
	switch n := len(gh); {
	case idx < n:
		u := gh[idx]
		if tok := copilot.GHToken(u); tok == "" || copilot.GitHubLogin(tok) != u {
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
	if user == "" {
		profile = signInProfile(d, "copilot") // gh's active account is copilot's own-login profile
	}
	if _, err := writeAgentField(d, "copilot", "PROFILE", profile); err != nil {
		d.printf("copilot: %v\n", err)
		return false
	}
	d.printf("%-10s runs as %s\n", "copilot", Bracket(user != "", user+" (pinned as "+profile+")", "gh's active account"))
	return true
}

// opencodeMenu is the home row's state only; its page is runOpencodePage (opencode_page.go).
func opencodeMenu(d *dialog) agentMenu {
	if opencode.OpencodeOwnLoginPresent() || opencode.OpencodeCopilotLoginPresent() {
		return agentMenu{state: agentState{modeSubscription, "", true}}
	}
	return agentMenu{state: agentState{mode: modeNone}}
}
