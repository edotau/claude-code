// home_menu.go — the top-level interactive `configure` picker: one row per agent CLI (mode · check), then Submit.
package menu

import (
	"maps"
	"os"
	"slices"
	"sync"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
)

// pickMenu is the home page's sectioned picker seam; tests replace it.
var pickMenu = tui.PickSections

// submitRow ends the menu: y saves the keys entered in it to the secrets manager, n discards them like Esc.
var submitRow = EnterRow("save keys to the secrets manager and exit")

// runMenu is `configure`'s interactive entry with no section named: the agents, tools and settings sections;
// picking a row runs it, then the page re-renders with fresh status, cursor kept, until Submit or Esc. --agent
// narrows it to that agent's row. Any change made here re-renders settings.json and the agent configs on exit.
func runMenu(d *dialog) int {
	d.pending = map[string]stagedKey{}
	defer func() {
		if d.changed {
			rerenderConfigs(d)
		}
	}()
	start := 0
	for {
		sections, actions := homeSections(d)
		idx, err := pickMenu(Heading("agents"), sections, submitRow, start)
		if err != nil {
			discardKeys(d)
			d.printf("configure: %v\n", err)
			return 1
		}
		if idx < 0 || idx == len(actions) { // Enter, Esc and Left all leave through the one prompt
			if confirmLeave(len(d.pending), "key(s) to the secrets manager") {
				ok := saveKeys(d)
				d.changed = true
				if !ok {
					return 1
				}
				return 0
			}
			discardKeys(d) // saveKeys never ran: nothing was drained, so discard every staged key
			return 0
		}
		start = idx
		actions[idx](d)
	}
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
	sections := []tui.Section{{Title: Heading("agents"), Rows: rows}}
	return append(sections, extra...), append(actions, acts...)
}

// extraSections is the tools and settings blocks; a seam so menu tests stay free of live probes.
var extraSections = func(d *dialog) ([]tui.Section, []func(*dialog)) {
	return Sections([]Block[*dialog]{{Title: "tools", Rows: toolsRows(d)}, {Title: "settings", Rows: settingsRows(d)}})
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
			if page, ok := agentPages[n]; ok {
				page(d)
				return
			}
			runAgentMenu(d, displayLabel(n), m)
		}}
	}
	return RowsOf(rows)
}

// agentPages are the agents whose row opens a tui.List page instead of runAgentMenu (opencode first).
var agentPages = map[string]func(*dialog){"opencode": runOpencodePage}

// stagedKey is one menu-entered key: the value Submit will store and the env it shadows until then.
type stagedKey struct {
	val, prev string
	had       bool
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
		if err := vault.Store(name, k.val); err != nil {
			d.printf("%s: not saved: %v\n", name, err)
			ok = false
			continue
		}
		d.printf("saved %s to the vault (%s)\n", name, vault.Redact(k.val))
		stored = append(stored, name)
	}
	for _, name := range stored {
		k := d.pending[name]
		if k.had {
			_ = os.Setenv(name, k.prev)
		} else {
			_ = os.Unsetenv(name)
		}
		delete(d.pending, name)
	}
	return ok
}

// discardKeys drops staged keys and restores the env each one shadowed (n / Esc leave nothing behind).
func discardKeys(d *dialog) {
	for name, k := range d.pending {
		if k.had {
			_ = os.Setenv(name, k.prev)
		} else {
			_ = os.Unsetenv(name)
		}
	}
	d.pending = nil
}
