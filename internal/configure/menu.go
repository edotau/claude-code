// menu.go — the top-level interactive `configure` picker: one row per agent CLI (mode · check), then Submit.
package configure

import (
	"maps"
	"os"
	"slices"

	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

// pickMenu is the home page's sectioned picker seam; tests replace it.
var pickMenu = tui.PickSections

// submitRow ends the menu: y saves the keys entered in it to the secrets manager, n discards them like Esc.
var submitRow = enterRow("save keys to the secrets manager and exit")

// runMenu is `configure`'s interactive entry with no section named: the agents, tools and settings sections;
// picking a row runs it, then the page re-renders with fresh status, cursor kept, until Submit or Esc. --agent
// narrows it to that agent's row. Any change made here re-renders settings.json and the vendor configs on exit.
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
		idx, err := pickMenu("== agents ==", sections, submitRow, start)
		if err != nil || idx < 0 || idx == len(actions) { // Enter, Esc and Left all leave through the one prompt
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

// homeSections is the agents block, then tools and settings (both hidden under --agent).
func homeSections(d *dialog) ([]tui.Section, []func(*dialog)) {
	rows, actions := menuRows(d)
	sections := []tui.Section{{Title: "== agents ==", Rows: rows}}
	if d.o.Vendor != "" {
		return sections, actions
	}
	extra, acts := extraSections(d)
	return append(sections, extra...), append(actions, acts...)
}

// extraSections is the tools and settings blocks; a seam so menu tests stay free of live probes.
var extraSections = func(d *dialog) ([]tui.Section, []func(*dialog)) {
	var sections []tui.Section
	var actions []func(*dialog)
	for _, sec := range []struct {
		title string
		rows  []homeRow
	}{{"== tools ==", toolsRows(d)}, {"== settings ==", settingsRows(d)}} {
		r, a := rowsOf(sec.rows)
		sections = append(sections, tui.Section{Title: sec.title, Rows: r})
		actions = append(actions, a...)
	}
	return sections, actions
}

// menuRows builds one row per agent (name · mode · check) and the submenu each opens, scoped to d.o.Vendor when set.
func menuRows(d *dialog) ([][2]string, []func(*dialog)) {
	var names []string
	var cols [][2]string
	var actions []func(*dialog)
	for _, name := range append([]string{"claude"}, vendors.Names()...) {
		build, ok := agentMenus[name]
		if !ok || (d.o.Vendor != "" && d.o.Vendor != name) {
			continue
		}
		st := build(d).state
		n := name
		names, cols = append(names, displayLabel(n)), append(cols, [2]string{modeLabel(d, n, st), st.check()})
		actions = append(actions, func(d *dialog) {
			if page, ok := agentPages[n]; ok {
				page(d)
				return
			}
			runAgentMenu(d, displayLabel(n), agentMenus[n](d))
		})
	}
	return withChecks(names, cols), actions
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
		if err := secrets.Store(name, k.val); err != nil {
			d.printf("%s: not saved: %v\n", name, err)
			ok = false
			continue
		}
		d.printf("saved %s to the vault (%s)\n", name, secrets.Redact(k.val))
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
