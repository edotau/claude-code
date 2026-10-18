// menu.go — the top-level interactive `configure` picker: one row per agent CLI (mode · check), then Exit.
package configure

import (
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

// pickMenu is the top-level menu's picker seam; tests replace it.
var pickMenu = tui.Pick

// exitRow closes the menu so leaving is discoverable without knowing Esc (which still works).
var exitRow = [2]string{"Exit", "leave configure [y/N]"}

// runMenu is `configure`'s interactive entry with no section named: a pick-one menu over claude and every agent
// CLI; picking a row opens that agent's submenu, then the menu re-renders with fresh status, cursor kept, until
// Exit (confirmed) or Esc. --agent narrows it to that one row.
func runMenu(d *dialog) int {
	start := 0
	for {
		rows, actions := menuRows(d)
		idx, err := pickMenu("== agents ==", append(rows, exitRow), start)
		if err != nil || idx < 0 {
			return 0
		}
		start = idx
		if idx == len(actions) {
			if actionConfirmed("Leave configure") {
				return 0
			}
			continue
		}
		actions[idx](d)
	}
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
		names, cols = append(names, n), append(cols, [2]string{st.label(), st.check()})
		actions = append(actions, func(d *dialog) { runAgentMenu(d, n, agentMenus[n](d)) })
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
