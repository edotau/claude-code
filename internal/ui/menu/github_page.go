package menu

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilotauth"
	"github.com/edotau/claude-code/internal/github"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/ansi"
)

// printRoute prints one agent.DescribeRoute result as a line under 80 columns.
func printRoute(d *dialog, ri agent.RouteInfo) {
	status, reason := "ok", ""
	if !ri.OK {
		status, reason = "FIX", strings.TrimRight("  "+ri.Reason, " ")
	}
	d.printf("%-9s route(%-8s) %-4s %s/%s%s\n", ri.Agent, ri.Route, status, ri.Provider, ri.Model, reason)
}

// githubStatusRows prints every github section status line and reports which are OK.
func githubStatusRows(d *dialog) (ghOK, copilotOK, routesOK bool) {
	gh := configure.GHAuthCheck()
	switch {
	case !gh.Found:
		d.printf("%-16s ABSENT: gh CLI not found on PATH\n", "gh CLI")
	case gh.LoggedIn:
		acct := github.GHActiveAccount()
		if acct == "" {
			acct = gh.Account
		}
		d.printf("%-16s present (active account %s)\n", "gh CLI", acct)
		ghOK = true
	default:
		d.printf("%-16s ABSENT: not logged in\n", "gh CLI")
	}

	copilotAccountLine(d)

	// copilot's own stored login; gh's active account never counts as it
	own := copilotauth.StoredLogin()
	copilotOK = own != ""
	if copilotOK {
		d.printf("%-16s present (logged in as %s)\n", "copilot CLI", own)
	} else {
		d.printf("%-16s ABSENT: not logged in (copilot login)\n", "copilot CLI")
	}

	d.printf("%-16s %s\n", "opencode login", opencodeAccountLine(d.reg))

	ctx := context.Background()
	oc, _ := agent.LookupSchema("opencode")
	cp, _ := agent.LookupSchema("copilot")
	ocRoute, cpRoute := oc.DescribeRoute(ctx, d.reg), cp.DescribeRoute(ctx, d.reg)
	printRoute(d, ocRoute)
	printRoute(d, cpRoute)
	return ghOK, copilotOK, ocRoute.OK && cpRoute.OK
}

// opencodeAccountLine is the GitHub account behind opencode's own Copilot sign-in in green, the other accounts dim.
func opencodeAccountLine(reg *providers.Registry) string {
	if !opencode.OpencodeCopilotLoginPresent() {
		return "not signed in (Log in to OpenCode)"
	}
	cur := github.GitHubLogin(opencode.OpencodeCopilotToken())
	if cur == "" {
		return "signed in, account not readable"
	}
	parts := []string{ansi.SpanOK + cur + ansi.SpanEnd}
	for _, a := range githubAccounts(reg) {
		if a != cur {
			parts = append(parts, ansi.SpanOff+a+ansi.SpanEnd)
		}
	}
	return strings.Join(parts, "  ")
}

// copilotAccountLine is the account copilot runs as: the pinned one, else what an unpinned dispatch would use.
func copilotAccountLine(d *dialog) {
	login, _ := providers.CopilotLogin(d.reg)
	if login == "" {
		a, ok := copilotauth.AuthSourceFor("")
		if !ok {
			login = "none signed in"
		} else {
			login = cmp.Or(a.Account, "token") + " (" + copilotauth.ShortSource(string(a.Source)) + ")"
		}
	}
	d.printf("%-16s %s\n", "copilot account", login)
}

// githubAccounts are the known GitHub logins: the github profiles' first, then gh's signed-in ones.
func githubAccounts(reg *providers.Registry) []string {
	var out []string
	if reg != nil {
		for _, n := range reg.ProfilesOf("github") {
			if l := reg.Providers[n].Login; l != "" && !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	for _, l := range github.GHAccounts() {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// githubProfile is the profile behind a GitHub account, named after it and written on first use.
func githubProfile(reg *providers.Registry, login string) (string, error) {
	if reg != nil {
		for _, n := range reg.ProfilesOf("github") {
			if reg.Providers[n].Login == login {
				return n, nil
			}
		}
	}
	return login, providers.AddGithubAccount(login)
}

// runGithub is the github section: opencode's Copilot fallback ladder (login state, routes; on a terminal a fix menu).
func runGithub(d *dialog) bool {
	if d.interactive() {
		return githubMenu(d)
	}
	ghOK, copilotOK, routesOK := githubStatusRows(d)
	if !ghOK {
		d.println("fix: gh auth login")
	}
	if !copilotOK {
		d.println("fix: copilot login")
	}
	return !d.o.Check || routesOK
}

// githubMenu loops a status report + action picker until Back/Esc.
func githubMenu(d *dialog) bool {
	login := func(name, detail, label string, check func() (bool, string), argv ...string) shellRow {
		return shellRow{Name: name, Detail: detail, Run: func() bool { runLoginChecked(d, argv, label, check); return false }}
	}
	as := func(label string) func() (bool, string) { return func() (bool, string) { return signedIn(label) } }
	d.shell().Run("GitHub (opencode fallback) — pick an action", func() []shellRow {
		githubStatusRows(d)
		return []shellRow{
			login("Log in to GitHub CLI", "gh auth login", "gh CLI", as("gh"), "gh", "auth", "login"),
			login("Log in to Copilot", "copilot login", "copilot", as("copilot"), "copilot", "login"),
			login("Log in to OpenCode", "opencode auth login (choose GitHub Copilot)", "opencode", opencodeSignedIn,
				"opencode", "auth", "login"),
			{Name: "Pick fallback models", Detail: "set github-copilot opus/sonnet/haiku for opencode",
				Run: func() bool { pickCopilotModels(d); return false }},
		}
	})
	return true
}

// opencodeSignedIn is the OpenCode row's check: opencode's own GitHub Copilot login, not any opencode login.
func opencodeSignedIn() (bool, string) {
	return opencode.OpencodeCopilotLoginPresent(), "GitHub Copilot login stored"
}

// pickCopilotModels picks a catalog id per alias ("auto" also needs an opencode_fallback) and writes the overlay.
func pickCopilotModels(d *dialog) {
	list, err := copilotCatalog(d)
	if err != nil {
		d.printf("copilot models: %v\n", err)
		return
	}
	ids := []string{"auto"}
	rows := [][2]string{{"auto", "resolve automatically; needs opencode_fallback"}}
	for _, m := range list {
		if m.Enabled {
			ids = append(ids, m.ID)
			rows = append(rows, [2]string{m.ID, m.Name})
		}
	}

	picks, ok := pickAliasModels(d, ids, rows)
	if !ok {
		return
	}
	fallback := ""
	if slices.Contains(slices.Collect(maps.Values(picks)), "auto") {
		idx, err := d.shell().Pick("github fallback — opencode_fallback (auto needs a concrete id)", rows[1:])
		if err != nil || idx < 0 {
			d.printf("github fallback: cancelled at opencode_fallback; nothing written\n")
			return
		}
		fallback = ids[idx+1]
	}

	if err := configure.WriteGithubCopilotModels(picks, fallback); err != nil {
		d.printf("github fallback: %v\n", err)
		return
	}
	d.changed = true
	d.reload()
	suffix := ""
	if fallback != "" {
		suffix = " opencode_fallback=" + fallback
	}
	d.printf("github fallback: wrote vault overlay/providers (opus=%s sonnet=%s haiku=%s%s)\n",
		picks[models.Opus], picks[models.Sonnet], picks[models.Haiku], suffix)
}

// pickAliasModels asks for a model id per opus/sonnet/haiku alias; false (after saying so) when any pick is cancelled.
func pickAliasModels(d *dialog, ids []string, rows [][2]string) (map[string]string, bool) {
	picks := map[string]string{}
	for _, alias := range []string{models.Opus, models.Sonnet, models.Haiku} {
		idx, err := d.shell().Pick("github fallback — "+alias+" model", rows)
		if err != nil || idx < 0 {
			d.printf("github fallback: cancelled at the %s model; nothing written\n", alias)
			return nil, false
		}
		picks[alias] = ids[idx]
	}
	return picks, true
}
