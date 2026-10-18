// menu.go — the top-level interactive `configure` picker: one row per agent CLI plus a status row per
// section, replacing the old Sections walk when no section is named on a terminal.
package configure

import (
	"cmp"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

// pickMenu is the top-level menu's picker seam; tests replace it.
var pickMenu = tui.Pick

// runMenu is `configure`'s interactive entry with no section named: a pick-one menu over claude,
// every agent CLI, and every section below it; picking a row runs that action, then the menu
// re-renders with fresh status, cursor kept, until Done or Esc. --agent narrows it to that one row.
func runMenu(d *dialog) int {
	start := 0
	for {
		rows, actions := menuRows(d)
		full := make([][2]string, len(rows)+1)
		copy(full, rows)
		full[len(rows)] = [2]string{"Done", "leave configure"}
		idx, err := pickMenu("configure", full, start)
		if err != nil || idx < 0 || idx == len(rows) {
			return 0
		}
		start = idx
		actions[idx](d)
	}
}

// menuRows builds the menu's rows and the action run when each is picked, scoped to d.o.Vendor when
// set — the same "--agent narrows to one agent CLI's own setup" contract as the keys/agents sections.
func menuRows(d *dialog) ([][2]string, []func(*dialog)) {
	var rows [][2]string
	var actions []func(*dialog)
	scope := d.o.Vendor
	if scope == "" || scope == "claude" {
		rows = append(rows, [2]string{"claude", claudeStatus(d)})
		actions = append(actions, func(d *dialog) {
			pickClaudeBackend(d, &firstParty{reg: d.reg}, currentProvider(d.reg))
		})
	}
	for _, name := range vendors.Names() {
		if scope != "" && scope != name {
			continue
		}
		n := name
		targets, err := keyTargets(d.reg, n)
		if err != nil || len(targets) == 0 {
			continue
		}
		krows, _ := keyRows(targets)
		rows = append(rows, krows[0])
		actions = append(actions, func(d *dialog) {
			kts, err := keyTargets(d.reg, n)
			if err != nil || len(kts) == 0 {
				d.println(err)
				return
			}
			changeKey(d, kts[0])
		})
	}
	return rows, actions
}

// claudeStatus is the claude row's detail: the pinned backend and, for the two first-party billing
// modes, whether its credential actually works right now.
func claudeStatus(d *dialog) string {
	cur := currentProvider(d.reg)
	fp := &firstParty{reg: d.reg}
	switch cur {
	case subscriptionName:
		ok, plan := fp.login()
		if !ok {
			return "OAuth login · subscription · not logged in"
		}
		line := "OAuth login · subscription" + plan
		if providers.HeadlessOAuthToken() != "" {
			line += " · headless token"
		}
		return line
	case anthropicRow:
		p, ok := d.reg.Providers[anthropicRow]
		if !ok || !fp.key() {
			return "API key · per-token usage · missing"
		}
		return "API key · per-token usage · " + cmp.Or(probeStatus(p), "unverified")
	case "chatgpt":
		return "OAuth login · ChatGPT plan"
	case "github-copilot":
		return "OAuth login · Copilot plan"
	}
	if p, ok := d.reg.Providers[cur]; ok && p.Auth.Env != "" {
		return "API key · per-token usage · " + cmp.Or(probeStatus(p), "unverified") + " · " + cur
	}
	return backendDetail(d.reg, cur)
}

// probeStatus live-checks p's credential (4 s cap) and returns "verified" / "rejected" / "unreachable" / "".
func probeStatus(p *providers.Provider) string {
	status, _ := probe(keyTarget{provider: p})
	if status == "login" {
		return ""
	}
	return status
}
