// home_menu.go — the top-level interactive `configure` picker: one row per agent CLI (mode · check), then Submit.
package menu

import (
	"cmp"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// pickMenu is the home page's sectioned picker seam; tests replace it.
var pickMenu = tui.PickSections

// submitRow ends the menu: y saves the keys entered in it to the secrets manager, n discards them like Esc.
var submitRow = enterRow("save keys to the secrets manager and exit")

// runMenu is `configure`'s interactive entry with no section named: the agents, tools and settings sections;
// picking a row runs it, then the page re-renders with fresh status, cursor kept, until Submit or Esc. --agent
// narrows it to that agent's row. Any change made here re-renders the agent configs on exit.
func runMenu(d *dialog) (code int) {
	d.pending = map[string]stagedKey{}
	defer func() {
		if d.changed {
			if err := rerenderConfigs(d); err != nil {
				d.println(err)
				code = 1
			}
		}
	}()
	start := 0
	for {
		sections, actions := homeSections(d)
		idx, err := pickMenu(heading("agents"), sections, submitRow, start)
		if err != nil {
			discardKeys(d)
			d.printf("configure: %v\n", err)
			return 1
		}
		if idx < 0 || idx == len(actions) { // Enter, Esc and Left all leave through the one prompt
			return leaveMenu(d)
		}
		start = idx
		actions[idx](d)
	}
}

// leaveMenu saves or discards the staged keys on leaving; 1 when a key save or a secret deletion failed.
func leaveMenu(d *dialog) int {
	if !confirmLeave(len(d.pending), "key(s) to the secrets manager") {
		discardKeys(d) // saveKeys never ran: nothing was drained, so discard every staged key
		return failedDeletes(d)
	}
	ok := saveKeys(d)
	d.changed = true
	if !ok {
		return 1
	}
	return failedDeletes(d)
}

// failedDeletes is 1 when staged secret deletions are still pending (their vault delete failed on the page).
func failedDeletes(d *dialog) int {
	if n := len(d.pendingDeletes); n > 0 {
		d.printf("secrets: %d deletion(s) not removed from the vault; retry from the secrets page\n", n)
		return 1
	}
	return 0
}

// homeSections is the agents block, then tools and settings (both hidden under --agent); the two build concurrently.
func homeSections(d *dialog) ([]tui.Section, []func(*dialog)) {
	var extra []tui.Section
	var acts []func(*dialog)
	var wg sync.WaitGroup
	if d.o.Agent == "" {
		wg.Go(func() { extra, acts = extraSections(d) })
	}
	rows, actions := menuRows(d)
	wg.Wait()
	sections := []tui.Section{{Title: heading("agents"), Rows: rows}}
	return append(sections, extra...), append(actions, acts...)
}

// extraSections is the tools and settings blocks; a seam so menu tests stay free of live probes.
var extraSections = func(d *dialog) ([]tui.Section, []func(*dialog)) {
	return sections([]block[*dialog]{{Title: "tools", Rows: toolsRows(d)}, {Title: "settings", Rows: settingsRows(d)}})
}

// menuRows builds one row per agent (name · mode · check) and the submenu each opens, scoped to d.o.Agent when set.
// Each agent's status checks (CLI auth, gh, key probes) run in their own goroutine; rows keep roster order.
func menuRows(d *dialog) ([][2]string, []func(*dialog)) {
	var names []string
	for _, name := range append([]string{"claude"}, agent.Names()...) {
		if _, ok := agentMenus[name]; ok && (d.o.Agent == "" || d.o.Agent == name) {
			names = append(names, name)
		}
	}
	menus := make([]agentMenu, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() { menus[i] = agentMenus[name](d) })
	}
	wg.Wait()
	rows := make([]homeRow, len(names))
	for i, n := range names {
		m := menus[i] // the page re-renders after every pick, so this build is fresh enough to open
		rows[i] = homeRow{Name: displayLabel(n), State: modeLabel(d, n, m.state), Check: m.state.check(), Run: func(d *dialog) {
			if n == "opencode" { // opencode opens a tui.List page instead of runAgentMenu
				runOpencodePage(d)
				return
			}
			runAgentMenu(d, displayLabel(n), m)
		}}
	}
	return rowsOf(rows)
}

// stagedKey is one menu-entered key: the value Submit will store and the env it shadows until then.
type stagedKey struct {
	val, prev string
	had       bool
}

// restore puts back the env value k shadowed, reporting a failure to d.
func (k stagedKey) restore(d *dialog, name string) {
	var err error
	if k.had {
		err = os.Setenv(name, k.prev)
	} else {
		err = os.Unsetenv(name)
	}
	if err != nil {
		d.printf("%s: env not restored: %v\n", name, err)
	}
}

// stage records name for Submit and exports it so status rows and probes in this run already see it.
func (d *dialog) stage(name, val string) bool {
	k, seen := d.pending[name]
	if !seen {
		k.prev, k.had = os.LookupEnv(name)
	}
	if err := os.Setenv(name, val); err != nil {
		d.printf("%s: not staged: %v\n", name, err)
		return false
	}
	k.val = val
	d.pending[name] = k
	return true
}

// saveKeys stores every staged key in the vault and reports each redacted; a name that fails to
// store stays in d.pending (and its shadowing env stays set) instead of being silently dropped.
// Returns false if any key failed, so the caller can make the menu's exit status reflect it.
func saveKeys(d *dialog) bool {
	if len(d.pending) == 0 {
		d.println("keys: nothing to save; no key was entered in this session")
		return true
	}
	ok := true
	stored := make([]string, 0, len(d.pending))
	for _, name := range slices.Sorted(maps.Keys(d.pending)) {
		k := d.pending[name]
		if err := configure.StoreSecret(name, k.val); err != nil {
			d.printf("%s: not saved: %v\n", name, err)
			ok = false
			continue
		}
		d.printf("saved %s to the vault (%s)\n", name, vault.Redact(k.val))
		stored = append(stored, name)
	}
	for _, name := range stored {
		d.pending[name].restore(d, name)
		delete(d.pending, name)
	}
	return ok
}

// discardKeys drops staged keys and restores the env each one shadowed (n / Esc leave nothing behind).
func discardKeys(d *dialog) {
	for name, k := range d.pending {
		k.restore(d, name)
	}
	d.pending = nil
}

// Labels section — an agent's mode column from agentPlans + agentPlanFormat (vault overlay/providers), else today's text.

// labelMax is the mode column's room in an 80-column row (name 11 · label · check 12, plus gaps).
const labelMax = 48

// agentMaker is the provider a plan names when its entry gives none.
var agentMaker = map[string]string{
	agentid.Claude: "Anthropic", agentid.Codex: "OpenAI", agentid.Gemini: "Google", agentid.Copilot: "GitHub", agentid.OpenCode: "opencode",
}

// modeLabel renders the mode column from agentPlanFormat with the matching agentPlans entry (tier and provider
// default to the detected plan and the agent's agent). No entry and no format keeps today's label.
func modeLabel(d *dialog, name string, st agentState) string {
	if d.reg == nil || (len(d.reg.AgentPlans) == 0 && d.reg.AgentPlanFormat == "") {
		return st.label()
	}
	plans, ok := d.reg.AgentPlans[name]
	if !ok {
		plans = d.reg.AgentPlans[displayLabel(name)] // "antigravity" names gemini too
	}
	id := rawPlanID(d, name, st)
	plan, matched := matchPlan(plans, id)
	if st.mode == modeNone { // a signed-out row holds no plan, whatever a "*" entry says
		plan, matched = providers.Plan{}, false
	}
	if !matched && d.reg.AgentPlanFormat == "" {
		return st.label()
	}
	format := d.reg.AgentPlanFormat
	if format == "" { // default: the mode, then the plan as its provider names it ("subscription (Google AI Pro)")
		return textutil.Truncate(withTier(st.mode, cmp.Or(plan.Tier, st.tier.Name)), labelMax)
	}
	out := strings.NewReplacer("{provider}", cmp.Or(plan.Provider, agentMaker[name]), "{mode}", st.mode,
		"{tier}", cmp.Or(plan.Tier, st.tier.Name), "{id}", id).Replace(format)
	out = strings.Join(strings.Fields(out), " ") // an empty placeholder leaves no double space
	return textutil.Truncate(out, labelMax)
}

// matchPlan picks the plan whose id equals the raw id, else the longest matching * glob.
func matchPlan(plans []providers.Plan, id string) (providers.Plan, bool) {
	best, found := providers.Plan{}, false
	for _, p := range plans {
		switch {
		case p.ID == id:
			return p, true
		case strings.Contains(p.ID, "*") && glob(p.ID, id) && (!found || len(p.ID) > len(best.ID)):
			best, found = p, true
		}
	}
	return best, found
}

// glob matches s against a pattern whose only wildcard is * (any run, / included).
func glob(pattern, s string) bool {
	re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
	ok, _ := regexp.MatchString(re, s)
	return ok
}

// withTier renders a mode with its tier when one is known: "subscription (Max 20x)".
func withTier(mode, tier string) string {
	if tier == "" {
		return mode
	}
	return mode + " (" + tier + ")"
}

// rawPlanID is what the agent's sign-in reports (claude rate tier, ChatGPT plan, copilot_plan); on an API key it
// is the account's opus model id, so a pattern can match a provider's prefixed ids.
func rawPlanID(d *dialog, name string, st agentState) string {
	if st.mode != modeSubscription {
		if name == "copilot" && d.reg != nil {
			model, _ := providers.CopilotModel(d.reg)
			return model
		}
		if p, claude := modelTarget(d, name); p != nil {
			return currentModel(d, p, claude, models.Opus)
		}
		return ""
	}
	return st.tier.Raw
}
