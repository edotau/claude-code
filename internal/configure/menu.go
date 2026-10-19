// menu.go — the top-level interactive `configure` picker: one row per agent CLI (mode · check), then Submit.
package configure

import (
	"maps"
	"os"
	"slices"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/secrets"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

// pickMenu is the home page's sectioned picker seam; tests replace it.
var pickMenu = tui.PickSections

// submitRow ends the menu: y saves the keys entered in it to the secrets manager, n discards them like Esc.
var submitRow = [2]string{"Submit", "save keys to the secrets manager [y/N]"}

// runMenu is `configure`'s interactive entry with no section named: the agents, tools and settings sections;
// picking a row runs it, then the page re-renders with fresh status, cursor kept, until Submit or Esc. --agent
// narrows it to that agent's row. Any change made here re-renders settings.json and the vendor configs on exit.
func runMenu(d *dialog) int {
	d.pending = map[string]stagedKey{}
	defer discardKeys(d)
	defer func() {
		if d.changed {
			rerenderConfigs(d)
		}
	}()
	start := 0
	for {
		sections, actions := homeSections(d)
		idx, err := pickMenu("== agents ==", sections, submitRow, start)
		if err != nil || idx < 0 {
			return 0
		}
		start = idx
		if idx == len(actions) {
			if actionConfirmed("Save keys to the secrets manager") {
				saveKeys(d)
				d.changed = true
			}
			return 0
		}
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
		names, cols = append(names, displayLabel(n)), append(cols, [2]string{st.label(), st.check()})
		actions = append(actions, func(d *dialog) { runAgentMenu(d, displayLabel(n), agentMenus[n](d)) })
	}
	return withChecks(names, cols), actions
}

// probeStatus live-checks p's credential (4 s cap) and returns "verified" / "rejected" / "unreachable" / "".
func probeStatus(p *providers.Provider) string {
	status, _ := probe(keyTarget{provider: p})
	if status == "login" {
		return ""
	}
	return status
}

// stagedKey is one menu-entered key: the value Submit will store and the env it shadows until then.
type stagedKey struct {
	val, prev      string
	forceFile, had bool
}

// stage records name for Submit and exports it so status rows and probes in this run already see it.
func (d *dialog) stage(name, val string, forceFile bool) {
	k, seen := d.pending[name]
	if !seen {
		k.prev, k.had = os.LookupEnv(name)
	}
	k.val, k.forceFile = val, forceFile
	d.pending[name] = k
	os.Setenv(name, val)
}

// saveKeys stores every staged key in the secrets manager (OS keyring, else the file plane) and reports each.
func saveKeys(d *dialog) {
	if len(d.pending) == 0 {
		d.println("keys: nothing to save; no key was entered in this session")
		return
	}
	for _, name := range slices.Sorted(maps.Keys(d.pending)) {
		k := d.pending[name]
		plane, err := secrets.Store(name, k.val, k.forceFile)
		if err != nil {
			d.printf("%s: not saved: %v\n", name, err)
			continue
		}
		d.printf("saved %s to %s (%s)\n", name, plane, secrets.Redact(k.val))
	}
	discardKeys(d)
}

// discardKeys drops staged keys and restores the env each one shadowed (n / Esc leave nothing behind).
func discardKeys(d *dialog) {
	for name, k := range d.pending {
		if k.had {
			os.Setenv(name, k.prev)
		} else {
			os.Unsetenv(name)
		}
	}
	d.pending = nil
}
