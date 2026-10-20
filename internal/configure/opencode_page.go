// opencode_page.go — the opencode submenu as a searchable account list (tui.List): every stored or env-backed
// connection, add one via `opencode auth login`, make one active (2.x), log one out (ctrl+d twice), plus the BYOK
// key and model rows the old submenu carried. Every other vendor keeps runAgentMenu.
package configure

import (
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

// Seams for the opencode page; tests replace them so no real opencode or terminal is touched.
var (
	opencodeAuth           = vendors.OpencodeAuth
	opencodeMajor          = vendors.OpencodeMajor
	opencodeLogoutArgv     = vendors.OpencodeLogoutArgv
	opencodeSwitchArgv     = vendors.OpencodeSwitchArgv
	invalidateOpencodeAuth = vendors.InvalidateOpencodeAuth
	opencodeList           = tui.List
	runAttached            = tui.RunAttached
	confirmKey             = tui.ConfirmKey
	lookOpencode           = paths.LookPathReal
)

// opencodeNames maps well-known opencode integration IDs to display names; others use the integration's Name.
var opencodeNames = map[string]string{
	"github-copilot": "GitHub Copilot", "openai": "OpenAI", "anthropic": "Anthropic", "google": "Google",
	"nvidia": "Nvidia", "databricks": "Databricks", "opencode": "OpenCode", "xai": "xAI",
}

func opencodeName(in vendors.OpencodeIntegration) string {
	if n, ok := opencodeNames[in.ID]; ok {
		return n
	}
	if in.Name != "" {
		return in.Name
	}
	return in.ID
}

// ocLogoutKey is ctrl+d: the page's double-press logout bind.
var ocLogoutKey = tui.Key{Kind: tui.KeyCtrl, Rune: 'd'}

// ocRow is one page row and what it stands for: an account (in+conn), the "not connected" filler, or an action.
type ocRow struct {
	row  tui.ListRow
	in   vendors.OpencodeIntegration
	conn *vendors.OpencodeConn
	run  func(d *dialog) // action rows only
}

// opencodePage is one render's data: title and rows.
type opencodePage struct {
	title string
	rows  []ocRow
	conns int
}

// ocDetail is an account row's middle column: the credential label, or $NAME for an env-backed one.
func ocDetail(c vendors.OpencodeConn) string {
	if c.Kind == "env" {
		return "$" + c.Label
	}
	return c.Label
}

// buildOpencodePage turns the adapter's data into rows; major is opencode's major version (0 unknown).
func buildOpencodePage(d *dialog, ints []vendors.OpencodeIntegration, major int) opencodePage {
	var p opencodePage
	for _, in := range ints {
		copilot := in.ID == "github-copilot" && len(in.Conns) > 0 // same rule as vendors.OpencodeCopilotLoginPresent
		for i := range in.Conns {
			c := in.Conns[i]
			var status []string
			switch {
			case c.Kind == "env":
				status = append(status, "environment · unset $"+c.Label+" to disconnect")
			case c.NeedsAuth != "":
				status = append(status, "sign in required")
			case c.Active:
				status = append(status, "active")
			}
			if copilot && c.Kind != "env" {
				status = append(status, "used by claude-code routes")
			}
			p.rows = append(p.rows, ocRow{row: tui.ListRow{
				Label: opencodeName(in), Detail: ocDetail(c), Status: strings.Join(status, " · "),
				OK: c.NeedsAuth == "", Section: "accounts",
			}, in: in, conn: &c})
			p.conns++
		}
	}
	if p.conns == 0 {
		p.rows = append(p.rows, ocRow{row: tui.ListRow{Label: "not connected", Section: "accounts"}})
	}
	p.rows = append(p.rows, ocRow{row: tui.ListRow{Label: "+ Add account", Detail: "opencode auth login · browser sign-in",
		Section: "actions"}, run: ocAddAccount})
	if d.reg != nil {
		for _, r := range []*billingRow{new(byokRow(d, "opencode")), modelRow(d, "opencode")} {
			if r == nil {
				continue
			}
			run := r.run
			p.rows = append(p.rows, ocRow{row: tui.ListRow{Label: r.label, Detail: r.desc, Section: "actions"},
				run: func(*dialog) { run() }})
		}
	}
	p.title = "opencode  not connected"
	if p.conns > 0 {
		p.title = fmt.Sprintf("opencode  ✓ %d %s", p.conns, plural(p.conns, "account", "accounts"))
	}
	if major > 0 {
		p.title += fmt.Sprintf(" · opencode %d.x", major)
	}
	return p
}

//go:fix inline
func ptr[T any](v T) *T { return new(v) }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// spec is the page's tui.List description.
func (p opencodePage) spec() tui.ListSpec {
	rows := make([]tui.ListRow, len(p.rows))
	for i, r := range p.rows {
		rows[i] = r.row
	}
	return tui.ListSpec{Title: p.title, Rows: rows, Search: true,
		Binds: []tui.Bind{{Key: ocLogoutKey, Name: "logout", Help: "log out", Confirm: "Press ctrl+d again to confirm"}}}
}

// runOpencodePage loops the page until Esc: re-read the logins, render, run the pick, keep the cursor.
func runOpencodePage(d *dialog) {
	start, lastErr := 0, ""
	for {
		ints, err := opencodeAuth()
		if err != nil && err.Error() != lastErr {
			lastErr = err.Error()
			d.printf("opencode: %v\n", err)
		}
		p := buildOpencodePage(d, ints, opencodeMajor())
		res, lerr := opencodeList(p.spec(), start)
		if lerr != nil || res.Index < 0 || res.Index >= len(p.rows) {
			return
		}
		start = res.Index
		r := p.rows[res.Index]
		switch {
		case r.conn == nil && r.run != nil:
			if res.Action == "" {
				r.run(d)
			}
		case r.conn == nil: // "not connected" filler
		case r.conn.Kind == "env":
			d.printf("%s: comes from $%s; unset it to disconnect\n", r.row.Label, r.conn.Label)
		case res.Action == "logout":
			ocLogout(d, r)
		default:
			ocAccountActions(d, r)
		}
	}
}

// ocAccountActions is the per-credential page: Make active (2.x, not already active), Log out, Back.
func ocAccountActions(d *dialog, r ocRow) {
	type act struct {
		label string
		run   func()
	}
	var acts []act
	if sw := opencodeSwitchArgv(r.in.ID, r.conn.ID); sw != nil && !r.conn.Active {
		acts = append(acts, act{"Make active", func() { ocSwitch(d, r, sw) }})
	}
	acts = append(acts, act{"Log out", func() {
		if confirmKey(fmt.Sprintf("Log out of %s (%s)?", r.row.Label, r.row.Detail), false) {
			ocLogout(d, r)
		}
	}}, act{"Back", func() {}})
	rows := make([]tui.ListRow, len(acts))
	for i, a := range acts {
		rows[i] = tui.ListRow{Label: a.label}
	}
	res, err := opencodeList(tui.ListSpec{Title: r.row.Label + " · " + r.row.Detail, Rows: rows}, 0)
	if err != nil || res.Index < 0 || res.Index >= len(acts) {
		return
	}
	acts[res.Index].run()
}

// ocLogout runs opencode's own non-interactive logout for one account (the double press or [y/N] already confirmed).
func ocLogout(d *dialog, r ocRow) {
	cancelled, err := runAttached(opencodeLogoutArgv(r.in.ID, r.conn.ID))
	invalidateOpencodeAuth()
	switch {
	case cancelled:
		d.printf("%s: logout cancelled\n", r.row.Label)
	case err != nil:
		d.printf("%s: %v\n", r.row.Label, err)
	default:
		d.changed = true // routing may follow a different login now: configs re-render on exit
		d.printf("Removed account from %s\n", r.row.Label)
	}
}

// ocSwitch makes r's credential the integration's active one (opencode 2.x).
func ocSwitch(d *dialog, r ocRow, argv []string) {
	cancelled, err := runAttached(argv)
	invalidateOpencodeAuth()
	switch {
	case cancelled:
		d.printf("%s: switch cancelled\n", r.row.Label)
	case err != nil:
		d.printf("%s: %v\n", r.row.Label, err)
	default:
		d.changed = true
		d.printf("%s: %s is now active\n", r.row.Label, r.row.Detail)
	}
}

// ocAddAccount delegates sign-in to the real opencode (never the harness shim) and reports the new account count.
func ocAddAccount(d *dialog) {
	bin, err := lookOpencode("opencode")
	if err != nil { // a bare "opencode" could only resolve to the shim, which would loop back into claude-code
		d.printf("opencode: %v\n", err)
		return
	}
	cancelled, err := runAttached([]string{bin, "auth", "login"})
	invalidateOpencodeAuth()
	switch {
	case cancelled:
		d.printf("opencode: sign-in cancelled\n")
	case err != nil:
		d.printf("opencode: %v\n", err)
	default:
		d.changed = true
		ints, _ := opencodeAuth()
		n := 0
		for _, in := range ints {
			n += len(in.Conns)
		}
		d.printf("opencode: %d %s\n", n, plural(n, "account", "accounts"))
	}
}
