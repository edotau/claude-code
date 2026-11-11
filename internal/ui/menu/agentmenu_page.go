package menu

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/discovery"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

const (
	modeSubscription = "subscription"

	modeAPIKey = "API key"

	modeNone = "not set up"
)

// agentState is how an agent bills today and whether that credential checked out.
type agentState struct {
	mode     string                 // modeSubscription, modeAPIKey, modeNone, or a non-anthropic backend's label
	tier     providers.Subscription // plan the sign-in reports (Name "Max 20x", "Plus", "Pro+"; Raw for agent_plans)
	verified bool
}

// label is the top menu's mode column: "subscription (Max 20x)", "API key" or "not set up".
func (s agentState) label() string {
	if s.mode == modeSubscription {
		return withTier(s.mode, s.tier.Name)
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
		return d.shell().Pick(title, rows, start)
	}
	secs, order := agentSections(rows)
	at := max(slices.Index(order, start), 0)
	idx, err := tui.PickSectionsBack(title, secs, at)
	if err != nil || idx < 0 {
		return -1, err
	}
	return order[idx], nil
}

// signInRows are the agent page rows that hold a credential; the rest are actions.
var signInRows = map[string]bool{"Subscription": true, "API key": true, "Login": true, "OAuth token": true, "GitHub user": true}

// agentSections groups an agent page's rows under sign-in, actions and profile; order maps a shown index to rows'.
func agentSections(rows [][2]string) ([]tui.Section, []int) {
	secs := []tui.Section{{Title: Heading("sign-in")}, {Title: Heading("actions")}, {Title: Heading("profile")}}
	idx := make([][]int, len(secs))
	for i, r := range rows {
		k := 1
		switch {
		case signInRows[r[0]]:
			k = 0
		case r[0] == "Profile":
			k = 2
		}
		secs[k].Rows, idx[k] = append(secs[k].Rows, r), append(idx[k], i)
	}
	var out []tui.Section
	var order []int
	for k, sec := range secs {
		if len(sec.Rows) > 0 {
			out, order = append(out, sec), append(order, idx[k]...)
		}
	}
	return out, order
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
	internal := internalAgentName(name)
	if r := modelRow(d, internal); r != nil {
		m.rows = append(m.rows, *r)
	}
	activateOnSignIn(d, internal, m.rows)
	m.rows = append(m.rows, agentPinRows(d, internal)...)
	if _, ok := setupLogins[internal]; ok {
		m.rows = append([]billingRow{setupRow(d, internal)}, m.rows...)
	}
	rows := make([][2]string, len(m.rows))
	start := 0
	for i, r := range m.rows {
		rows[i] = [2]string{r.label, r.desc}
		if r.key != "" && r.key == m.state.mode {
			start = i
		}
	}
	idx, err := pickAgent(d, agentMenuTitle(name, m.state), rows, start)
	if err != nil || idx < 0 {
		return false
	}
	return m.rows[idx].run()
}

// internalAgentName undoes displayLabel: the page title carries the display label, the roster the internal name.
func internalAgentName(name string) string {
	internal := name
	for _, s := range agent.Roster() {
		if s.CLI.DisplayName == name {
			internal = s.Name
		}
	}
	return internal
}

// activateOnSignIn wraps each key or Login row so a successful sign-in switches the agent back on.
func activateOnSignIn(d *dialog, internal string, rows []billingRow) {
	for i, r := range rows {
		if run := r.run; r.key != "" || r.label == "Login" {
			rows[i].run = func() bool {
				ok := run()
				if ok {
					agentActiveOn(d, internal)
				}
				return ok
			}
		}
	}
}

// agentMenuTitle is "<name> (current: <mode>[ · verified])".
func agentMenuTitle(name string, st agentState) string {
	title := name + " (current: " + st.mode
	if st.verified {
		title += " · verified"
	}
	return title + ")"
}

// signIn runs an agent's own sign-in CLI (picking its row was the confirmation) and reports the outcome.
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
	p, ok := d.reg.KeyPresent(provider)
	return billingRow{modeAPIKey, desc + " " + Bracket(ok, "key set", "no key"), modeAPIKey, func() bool {
		if p == nil {
			d.printf("%s: no %s provider registered\n", label, provider)
			return false
		}
		kt := configure.KeyTarget{Label: label, EnvVar: p.Auth.Env, FixCmd: "claude-code vault set " + p.Auth.Env, Provider: p}
		return ensureKey(d, kt, ok)
	}}
}

// ownLoginState is the home-row state of an agent whose own sign-in is its subscription; plan only when present.
func ownLoginState(name string) (agentState, providers.AuthState) {
	st := agent.AuthStatus(name)
	if !st.Present {
		return agentState{mode: modeNone}, st
	}
	return agentState{modeSubscription, agent.AuthPlan(context.Background(), name, st), true}, st
}

// loginOrKey is the state of an agent that bills its own login first, else a BYOK key.
func loginOrKey(d *dialog, loggedIn bool, tier providers.Subscription) agentState {
	if loggedIn {
		return agentState{modeSubscription, tier, true}
	}
	// BYOK fallback (agent.DescribeRoute order): the anthropic key, handed over as generated config, never `auth login`.
	if p, ok := d.reg.KeyPresent(anthropicProvider); ok {
		return agentState{mode: modeAPIKey, verified: configure.KeyVerified(p)}
	}
	return agentState{mode: modeNone}
}

func codexMenu(d *dialog) agentMenu {
	st, auth := ownLoginState("codex")
	loggedIn := auth.Present
	if !loggedIn {
		if kt := configure.AgentKeyTarget(d.reg, "codex"); kt.Present {
			st = agentState{mode: modeAPIKey, verified: kt.Provider == nil || configure.KeyVerified(kt.Provider)}
		}
	}
	key, ensure := codexKeyRow(d, loggedIn)
	pinned := vault.GetAgentRecord("codex").Profile
	return agentMenu{st, []billingRow{
		{"Subscription", "ChatGPT login · plan limits " + Bracket(loggedIn, withTier("logged in", st.tier.Name), "not logged in"), modeSubscription, func() bool {
			if loggedIn {
				d.printf("%-10s on its ChatGPT login (kept)\n", "codex")
				return true
			}
			return signIn(d, "codex", []string{"codex", "login"})
		}},
		key,
		{"Login", "browser, headless or API key sign-in", "", func() bool { return codexLogin(d, ensure) }},
		{"Settings", "reasoning effort, verbosity", "", func() bool { return runCodexSettings(d) }},
		{"Models", "discover what " + cmp.Or(pinned, "the pinned profile") + " serves", "", func() bool { return codexModels(d, pinned) }},
	}}
}

// codexKeyRow is the OPENAI_API_KEY row, switching a ChatGPT login to the key once confirmed; ensure is the bare setup.
func codexKeyRow(d *dialog, loggedIn bool) (key billingRow, ensure func() bool) {
	key = keyRow(d, "codex", "openai", "pay per token (OPENAI_API_KEY)")
	ensure = key.run
	key.run = func() bool {
		if !ensure() {
			return false
		}
		if loggedIn && ConfirmTerminal("Switch codex from its ChatGPT login to OPENAI_API_KEY (`codex login --with-api-key`)", false) {
			return codexUseKey(d)
		}
		return true
	}
	return key, ensure
}

// codexModels previews the menu shared by opencode, copilot and codex merged with what profile serves, writes it only
// after a confirm, then lists the ids codex can use over Responses.
func codexModels(d *dialog, profile string) bool {
	if profile == "" {
		d.printf("codex: no profile pinned; set one on the Profile row\n")
		return false
	}
	res, err := discovery.Models(context.Background(),
		discovery.Query{Agent: agentid.OpenCode, Profile: profile, Mode: discovery.Verified})
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	pl, err := catalog.MenuSync(res.Provider, *res.Snapshot, catalog.SyncOptions{})
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	d.printf("codex: %s serves %d chat models, %d not in the shared menu\n", profile, len(pl.IDs), len(pl.Changes))
	for _, c := range pl.Changes {
		d.printf("+ %s  responses=%v\n", c.Model.ID, c.Model.Responses)
	}
	menuNow, applied := pl.Menu, false
	if len(pl.Changes) > 0 && ConfirmTerminal(fmt.Sprintf("Add %d model(s) to the menu shared with opencode and copilot", len(pl.Changes)), false) {
		if !sealCodexMenu(d, pl) {
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

// sealCodexMenu seals pl and stores its merged menu, printing the failure and returning false when either step errs.
func sealCodexMenu(d *dialog, pl catalog.SyncPlan) bool {
	_, err := pl.Seal()
	if err == nil {
		err = providers.SetOpencodeMenu(pl.Next)
	}
	if err != nil {
		d.printf("codex: %v\n", err)
		return false
	}
	return true
}

// codexKeyLogin feeds key to `codex login --with-api-key` on stdin.
var codexKeyLogin = func(key string) error {
	bin, err := paths.AgentBin("codex")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second) // key is piped, no prompt; 60s covers the network check
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "login", "--with-api-key")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(key), os.Stdout, os.Stderr
	return cmd.Run()
}

// codexLogin picks codex's sign-in method: ChatGPT in a browser, ChatGPT by device code, or a pasted API key.
func codexLogin(d *dialog, ensureKey func() bool) bool {
	idx, err := d.shell().Pick("codex — sign in", [][2]string{
		{"ChatGPT (browser)", "codex login"},
		{"ChatGPT (headless)", "codex login --device-auth"},
		{"API key", "paste OPENAI_API_KEY, then codex login --with-api-key"},
	})
	switch {
	case err != nil || idx < 0:
		return false
	case idx == 0:
		return signIn(d, "codex", []string{"codex", "login"})
	case idx == 1:
		return signIn(d, "codex", []string{"codex", "login", "--device-auth"})
	}
	return ensureKey() && codexUseKey(d)
}

// codexUseKey points codex's own auth at the stored OPENAI_API_KEY.
func codexUseKey(d *dialog) bool {
	key, ok := "", false
	if staged, isStaged := d.pending["OPENAI_API_KEY"]; isStaged { // pasted on this page, not yet in the vault
		key, ok = staged.val, true
	} else {
		key, _, ok = vault.Get("OPENAI_API_KEY")
	}
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
	auth := agent.AuthStatus("copilot")
	loggedIn := auth.Present
	plan := agent.AuthPlan(context.Background(), "copilot", auth)
	auth.Account = cmp.Or(auth.Account, plan.Account)
	st := loginOrKey(d, loggedIn, plan)
	if auth.Source == copilot.SrcStoredToken {
		st = agentState{mode: modeAPIKey, verified: auth.Account != ""}
	}
	chain := namedRouteChain(d, auth.Account)
	if len(chain) > 0 && chain[0].When == "pinned" && chain[0].OK { // a usable pinned Databricks profile is the route
		st = agentState{mode: backendLabel(d.reg, vault.GetAgentRecord("copilot").Profile), verified: true}
	}
	who := "gh active"
	if auth.Account != "" {
		who = auth.Account + " · " + copilot.ShortSource(auth.Source)
	}
	rows := []billingRow{
		{"Subscription", strings.TrimSpace("GitHub Copilot "+plan.Name) + " " + Bracket(loggedIn, who, "not logged in"), modeSubscription, func() bool {
			if loggedIn {
				d.printf("%-10s runs as %s (kept)\n", "copilot", auth.Label())
				return true
			}
			return signIn(d, "copilot", []string{"copilot", "login"})
		}},
		copilotKeyRow(d),
		{"Login", "browser sign-in (copilot login)", "", func() bool {
			return signIn(d, "copilot", []string{"copilot", "login"})
		}},
		{"GitHub user", "account the GitHub route uses " + Bracket(true, who, ""), "", func() bool {
			return pickGitHubUser(d, auth)
		}},
	}
	if fb := copilotFallback(chain); fb != "" {
		rows = append(rows, billingRow{"Fallback", fb, "", func() bool {
			for i, s := range chain {
				d.printf("%d. %-24s %-22s %s\n", i+1, s.Label, s.When, Bracket(s.OK, "usable", "not set up"))
			}
			return false
		}})
	}
	return agentMenu{st, rows}
}

// namedRouteChain is copilot's route chain with the GitHub step labelled by account (the offline chain cannot name a
// token-only account).
func namedRouteChain(d *dialog, account string) []copilot.RouteStep {
	chain := copilot.RouteChain(d.reg)
	for i := range chain {
		if chain[i].Label == "GitHub" && account != "" {
			chain[i].Label += " " + account
		}
	}
	return chain
}

// copilotFallback is the route chain after the first usable step ("GitHub edotau (under 95% of credits) → ..."); "" when none.
func copilotFallback(chain []copilot.RouteStep) string {
	first := slices.IndexFunc(chain, func(s copilot.RouteStep) bool { return s.OK })
	var rest []string
	for _, s := range chain[first+1:] {
		if s.OK {
			label := s.Label
			if s.When != "fallback" { // a plain fallback step needs no note; a conditional one says when
				label += " (" + s.When + ")"
			}
			rest = append(rest, label)
		}
	}
	return strings.Join(rest, " → ")
}

// copilotEffortChoices: menu label → the --reasoning-effort value copilot's CLI and SDK accept. Fixed
// fallback for when the SDK catalog is unavailable; a reachable catalog drives the row from the model's
// own SupportedReasoningEfforts instead (copilotEffortRow).
var copilotEffortChoices = [][2]string{
	{"Low", string(agentid.EffortLow)}, {"Medium", string(agentid.EffortMedium)}, {"High", string(agentid.EffortHigh)},
	{"Extra High", string(agentid.EffortXHigh)}, {"Max", string(agentid.EffortMax)},
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
		label := choiceLabel(copilotEffortChoices, e)
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
	desc := "context window on every launch " + Bracket(cur != "", choiceLabel(copilotContextChoices, cur), "auto")
	r := billingRow{"Context", desc, "", func() bool {
		return pickCopilotPin(d, "context", "CONTEXT", copilotContextChoices, cur)
	}}
	return &r
}

// choiceLabel is val's menu label from choices (label, value pairs); val itself if unknown.
func choiceLabel(choices [][2]string, val string) string {
	for _, c := range choices {
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
	rows = append(rows, [2]string{"unset", "copilot's own default" + Mark(cur == "")})
	for _, c := range choices {
		rows = append(rows, [2]string{c[0], c[1] + Mark(c[1] == cur)})
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
	env := providers.CopilotTokenEnv
	_, src, ok := vault.Get(env)
	stored := ok && src != "env"
	return billingRow{modeAPIKey, "GitHub token (" + env + ") " + Bracket(stored, "token set", "no token"), modeAPIKey, func() bool {
		kt := configure.KeyTarget{Label: "copilot", EnvVar: env, FixCmd: "claude-code vault set " + env}
		if stored {
			return ensureKey(d, kt, true)
		}
		return runAuthOption(d, kt, configure.AuthOption{Action: "Paste API key", Env: env})
	}}
}

// pickGitHubUser lists gh's accounts; a pick is validated (gh has a token GitHub accepts for that user) and pins
// only the name, COPILOT_PROFILE — launches fetch the token with `gh auth token --user`.
func pickGitHubUser(d *dialog, cur providers.AuthState) bool {
	gh := copilot.GHAccounts()
	if !d.onTerminal() {
		d.printf("copilot: runs as %s; gh accounts: %s\n", cur.Label(), strings.Join(gh, ", "))
		return false
	}
	active := copilot.GHActiveAccount()
	rows := ghUserRows(gh, active, cur.Account)
	rows = append(rows, [2]string{"New sign-in", "gh auth login · adds another account to this list"})
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
	default:
		return signIn(d, "gh", []string{"gh", "auth", "login"})
	}
}

// ghUserRows is one picker row per gh account, tagged personal/enterprise, gh active and in use.
func ghUserRows(gh []string, active, inUse string) [][2]string {
	var rows [][2]string
	for _, u := range gh {
		desc := "personal"
		if strings.Contains(u, "_") { // only enterprise managed users carry the _shortcode suffix
			desc = "enterprise"
		}
		if u == active {
			desc += " · gh active"
		}
		if u == inUse {
			desc += " · in use"
		}
		rows = append(rows, [2]string{u, desc})
	}
	return rows
}

// pinCopilotProfile pins the github profile whose login is user; "" is refused (gh's active account is GitHub auth).
// With no such row it sets login on the personal row when that has none, else asks for a profile with that login.
func pinCopilotProfile(d *dialog, user string) bool {
	if user == "" {
		d.printf("copilot: pick an account; gh's active account is GitHub auth (configure github)\n")
		return false
	}
	reg, err := providers.Load()
	if err != nil {
		d.printf("copilot: %v\n", err)
		return false
	}
	profile := providers.GithubAccountName(reg, user)
	if profile == user {
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
	if _, err := writeAgentField(d, "copilot", "PROFILE", profile); err != nil {
		d.printf("copilot: %v\n", err)
		return false
	}
	d.printf("%-10s runs as %s (pinned as %s)\n", "copilot", user, profile)
	return true
}

// opencodeMenu is the home row's state only; its page is runOpencodePage (opencode_page.go).
func opencodeMenu(d *dialog) agentMenu {
	if opencode.OpencodeOwnLoginPresent() { // Raw matches the agentPlans entry naming the Go plan
		return agentMenu{state: agentState{modeSubscription, providers.Subscription{Raw: "opencode-go"}, true}}
	}
	if opencode.OpencodeCopilotLoginPresent() {
		return agentMenu{state: agentState{modeSubscription, providers.Subscription{}, true}}
	}
	return agentMenu{state: agentState{mode: modeNone}}
}

// runAgy is AgySignIn with its message printed on d.
func runAgy(d *dialog) bool {
	ok, msg := configure.AgySignIn()
	d.printf("%s\n", msg)
	return ok
}

func geminiMenu(d *dialog) agentMenu {
	st, auth := ownLoginState("gemini")
	loginSel := auth.Present
	return agentMenu{st, []billingRow{
		{"Subscription", "Google login via agy · plan limits " + Bracket(loginSel, "logged in", "not logged in"), modeSubscription, func() bool {
			if loginSel {
				d.printf("%-10s on its Google login (kept)\n", "agy")
				return true
			}
			return runAgy(d)
		}},
		{"Login", "browser sign-in (agy)", "", func() bool {
			return runAgy(d)
		}},
	}}
}

// setupLogins is each agent's existing sign-in path; an agent absent here has no Set up row.
var setupLogins = map[string]func(d *dialog) error{
	"claude": func(*dialog) error { return configure.RunLogin([]string{"claude", "auth", "login"}) },
	"codex":  func(*dialog) error { return configure.RunLogin([]string{"codex", "login"}) },
	"gemini": func(d *dialog) error {
		if !runAgy(d) {
			return errors.New("agy sign-in did not complete")
		}
		return nil
	},
	"opencode": func(*dialog) error { return opencodeSignIn() },
	"copilot":  func(*dialog) error { return configure.RunLogin([]string{"copilot", "login"}) },
}

var (
	setupState = func(d *dialog, agent string) agentState { return agentMenus[agent](d).state }

	setupVerify = configure.VerifyAgent

	setupRoute = func(d *dialog, agent string) (bool, error) {
		return pickAgentProfile(d, agent, vault.GetAgentRecord(agent).Profile)
	}
)

// setupRow is the page's first row.
func setupRow(d *dialog, agent string) billingRow {
	return billingRow{label: "Set up", desc: "sign in, verify, pin the route", run: func() bool {
		agentSetUp(d, agent)
		return true
	}}
}

// agentSetUp signs in (skipped when already verified), sends one small real request and offers the route pin.
func agentSetUp(d *dialog, agent string) {
	name := displayLabel(agent)
	stop := func(step string, err error) {
		d.printf("%s: set up stopped at %s: %s\n", name, step, textutil.RedactSecrets(err.Error()))
	}
	if setupState(d, agent).verified {
		d.printf("%s: already signed in\n", name)
	} else {
		if err := setupLogins[agent](d); err != nil {
			stop("sign-in", err)
			return
		}
		d.changed = true
		agentActiveOn(d, agent)
	}
	if err := setupVerify(context.Background(), agent); err != nil {
		if agent == "copilot" && configure.QuotaFailure(err) {
			err = fmt.Errorf("signed in, but copilot has no credits left, so this is not a login failure (%w)", err)
		}
		stop("verify", err)
		return
	}
	d.printf("%s: verified with one live request\n", name)
	changed, err := setupRoute(d, agent)
	switch {
	case err != nil:
		stop("route", err)
	case !changed:
		d.printf("%s: signed in; the route was not changed\n", name)
	}
}
