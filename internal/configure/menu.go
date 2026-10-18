// menu.go — the top-level interactive `configure` picker: one row per agent CLI plus a status row per
// section, replacing the old Sections walk when no section is named on a terminal.
package configure

import (
	"context"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/browser"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/tailscale"
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
	if scope == "" {
		rows = append(rows,
			[2]string{"Models", modelsStatus(d)},
			[2]string{"Router", routerStatus(d)},
			[2]string{"GitHub", githubMenuStatus()},
			[2]string{"Remote", remoteMenuStatus()},
			[2]string{"Browser", browserMenuStatus()},
			[2]string{"Doctor", "run checks"},
		)
		actions = append(actions,
			func(d *dialog) { d.header("models"); runModels(d) },
			func(d *dialog) { d.header("router"); runRouter(d) },
			func(d *dialog) { d.header("github"); runGithub(d) },
			func(d *dialog) { d.header("remote"); runRemote(d) },
			func(d *dialog) { d.header("browser"); runBrowser(d) },
			func(d *dialog) { d.header("doctor"); launch.Doctor(d.out) },
		)
	}
	return rows, actions
}

// claudeStatus is the claude row's detail: the pinned backend and, for the two first-party billing
// modes, whether its credential actually works right now.
func claudeStatus(d *dialog) string {
	cur := currentProvider(d.reg)
	if isAnthropicBilling(cur) {
		detail := anthropicDetail(&firstParty{reg: d.reg}, cur)
		if cur == subscriptionName && providers.HeadlessOAuthToken() != "" {
			detail += " · token set"
		}
		return detail
	}
	return backendDetail(d.reg, cur)
}

// modelsStatus is the Models row's detail: the resolved wire model per slot.
func modelsStatus(d *dialog) string {
	sel, err := providers.Select(d.reg, "")
	if err != nil {
		return "error: " + err.Error()
	}
	parts := make([]string, 0, len(models.Slots))
	for _, s := range models.Slots {
		if _, ok := sel.Slots[s]; ok {
			parts = append(parts, s+"="+sel.ClientModel(s))
		}
	}
	return strings.Join(parts, " ")
}

// routerStatus is the Router row's detail: not needed, off, or already running.
func routerStatus(d *dialog) string {
	sel, err := providers.Select(d.reg, "")
	if err != nil {
		return "error"
	}
	if !sel.NeedsRouter(d.reg) {
		return "not needed"
	}
	if _, running := router.Running(context.Background()); running {
		return "running"
	}
	return "off"
}

// githubMenuStatus is the GitHub row's detail: the gh CLI's own login state.
func githubMenuStatus() string {
	if ghAuthCheck().loggedIn {
		return "logged in"
	}
	return "not logged in"
}

// remoteMenuStatus is the Remote row's detail: tailscale connectivity.
func remoteMenuStatus() string {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if _, err := tailscale.Status(ctx); err == nil {
		return "tailscale running"
	}
	return "tailscale not connected"
}

// browserMenuStatus is the Browser row's detail: the system default browser and its extension state.
func browserMenuStatus() string {
	rep := browser.Detect(browser.NewEnv())
	if rep.Default == "" {
		return "no default browser detected"
	}
	ext := ""
	for _, b := range rep.Browsers {
		if b.Default {
			ext = b.Paths.Extension
		}
	}
	return rep.Default + " default · extension " + ext
}
