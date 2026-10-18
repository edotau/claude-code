// menu.go — the top-level interactive `configure` picker: one row per agent CLI plus a status row per
// section, replacing the old Sections walk when no section is named on a terminal.
package configure

import (
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

// pickMenu is the top-level menu's picker seam; tests replace it.
var pickMenu = tui.Pick

// runMenu is `configure`'s interactive entry with no section named: a pick-one menu over claude and every agent
// CLI; picking a row runs that action, then the menu re-renders with fresh status, cursor kept, until Esc.
// --agent narrows it to that one row.
func runMenu(d *dialog) int {
	start := 0
	for {
		rows, actions := menuRows(d)
		idx, err := pickMenu("== agents ==", rows, start)
		if err != nil || idx < 0 {
			return 0
		}
		start = idx
		actions[idx](d)
	}
}

// menuRows builds the menu's rows and the action run when each is picked, scoped to d.o.Vendor when
// set — the same "--agent narrows to one agent CLI's own setup" contract as the keys/agents sections.
func menuRows(d *dialog) ([][2]string, []func(*dialog)) {
	var names []string
	var cols [][2]string
	var actions []func(*dialog)
	scope := d.o.Vendor
	if scope == "" || scope == "claude" {
		names, cols = append(names, "claude"), append(cols, claudeCols(d))
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
		names, cols = append(names, n), append(cols, keyCols(targets[0]))
		actions = append(actions, func(d *dialog) {
			kts, err := keyTargets(d.reg, n)
			if err != nil || len(kts) == 0 {
				d.println(err)
				return
			}
			changeKey(d, kts[0])
		})
	}
	return withChecks(names, cols), actions
}

// claudeCols is the claude row's mode (the pinned backend's sign-in · billing) and check.
func claudeCols(d *dialog) [2]string {
	cur := currentProvider(d.reg)
	fp := &firstParty{reg: d.reg}
	switch cur {
	case subscriptionName:
		ok, plan := fp.login()
		if !ok {
			return [2]string{"subscription", "not verified"}
		}
		return [2]string{"subscription" + plan, "verified"}
	case anthropicRow:
		p, ok := d.reg.Providers[anthropicRow]
		if !ok || !fp.key() {
			return [2]string{"per-token usage", "not verified"}
		}
		return [2]string{"per-token usage", verifiedOr(probeStatus(p))}
	case "chatgpt":
		return [2]string{"subscription", loginCheck(vendors.CodexChatGPTLoginPresent())}
	case "github-copilot":
		return [2]string{"subscription", loginCheck(vendors.CopilotLoginPresent())}
	}
	if p, ok := d.reg.Providers[cur]; ok && p.Auth.Env != "" {
		return [2]string{"per-token usage · " + cur, verifiedOr(probeStatus(p))}
	}
	return [2]string{backendDetail(d.reg, cur), ""}
}

func loginCheck(ok bool) string {
	if ok {
		return "verified"
	}
	return "not verified"
}

// probeStatus live-checks p's credential (4 s cap) and returns "verified" / "rejected" / "unreachable" / "".
func probeStatus(p *providers.Provider) string {
	status, _ := probe(keyTarget{provider: p})
	if status == "login" {
		return ""
	}
	return status
}

// verifiedOr collapses a probe result to the menu's two checks: "verified" or "not verified".
func verifiedOr(status string) string {
	if status == "verified" {
		return status
	}
	return "not verified"
}
