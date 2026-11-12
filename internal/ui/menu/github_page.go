package menu

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/vault"
)

// printRoute prints one agent.DescribeRoute result as a line under 80 columns: provider/model, the reason only
// on FIX; never a credential.
func printRoute(d *dialog, ri agent.RouteInfo) {
	status, reason := "ok", ""
	if !ri.OK {
		status, reason = "FIX", strings.TrimRight("  "+ri.Reason, " ")
	}
	d.printf("%-9s route(%-8s) %-4s %s/%s%s\n", ri.Agent, ri.Route, status, ri.Provider, ri.Model, reason)
}

// githubStatusRows prints every github section status line and reports which are OK, for both the
// interactive menu (re-run each loop) and the piped report (run once).
func githubStatusRows(d *dialog) (ghOK, copilotOK, routesOK bool) {
	gh := configure.GHAuthCheck()
	switch {
	case !gh.Found:
		d.printf("%-16s ABSENT: gh CLI not found on PATH\n", "gh CLI")
	case gh.LoggedIn:
		acct := copilot.GHActiveAccount()
		if acct == "" {
			acct = gh.Account
		}
		d.printf("%-16s present (active account %s)\n", "gh CLI", acct)
		ghOK = true
	default:
		d.printf("%-16s ABSENT: not logged in\n", "gh CLI")
	}

	d.printf("%-16s %s\n", "GitHub auth", githubAuthLine(d))

	auth := agent.AuthStatus("copilot")
	copilotOK = auth.Present
	if copilotOK {
		auth.Account = cmp.Or(auth.Account, agent.AuthPlan(context.Background(), "copilot", auth).Account)
		d.printf("%-16s present (%s)\n", "copilot CLI", auth.Label())
	} else {
		d.printf("%-16s ABSENT: no usable GitHub login\n", "copilot CLI")
	}

	d.printf("%-16s %s\n", "opencode login", opencodeAccountLine(d.reg))

	oc, _ := agent.LookupSchema("opencode")
	cp, _ := agent.LookupSchema("copilot")
	ocRoute, cpRoute := oc.DescribeRoute(d.reg), cp.DescribeRoute(d.reg)
	printRoute(d, ocRoute)
	printRoute(d, cpRoute)
	return ghOK, copilotOK, ocRoute.OK && cpRoute.OK
}

// opencodeAccountLine is the GitHub account behind opencode's own Copilot sign-in in green, the other accounts dim.
func opencodeAccountLine(reg *providers.Registry) string {
	if !opencode.OpencodeCopilotLoginPresent() {
		return "not signed in (Log in to OpenCode)"
	}
	cur := copilot.GitHubLogin(opencode.OpencodeCopilotToken())
	if cur == "" {
		return "signed in, account not readable"
	}
	parts := []string{ansi.SpanOK + cur + ansi.SpanEnd}
	for _, a := range GitHubAccounts(reg) {
		if a != cur {
			parts = append(parts, ansi.SpanOff+a+ansi.SpanEnd)
		}
	}
	return strings.Join(parts, "  ")
}

// githubAuthLine is the pinned GitHub account and whether gh's active account matches it.
func githubAuthLine(d *dialog) string {
	profile := vault.GitHubAuth()
	if profile == "" {
		return "follows gh's active account"
	}
	var p *providers.Provider
	if d.reg != nil {
		p = d.reg.Providers[profile]
	}
	switch {
	case p == nil || p.Provider != "github":
		return profile + " · drift: pin names no github profile, gh follows its active account"
	case p.Login == "":
		return profile + " · drift: profile has no login, gh follows its active account"
	}
	if active := copilot.GHActiveAccount(); active != p.Login {
		return p.Login + " · drift: gh active is " + cmp.Or(active, "none")
	}
	return p.Login
}

// pickGitHubAuth lists the GitHub accounts plus "follow gh active"; a pick switches gh, then stores it.
func pickGitHubAuth(d *dialog) {
	accounts := GitHubAccounts(d.reg)
	rows := append([][2]string{{"follow gh active", "no switch; use whichever account gh has active"}},
		ghUserRows(accounts, copilot.GHActiveAccount(), "")...)
	idx, err := d.shell().Pick("GitHub auth — gh's active account", rows)
	if err != nil || idx < 0 {
		return
	}
	if idx == 0 {
		applyGitHubAuth(d, "")
		return
	}
	applyGitHubAccount(d, accounts[idx-1])
}

// GitHubAccounts are the logins `--auth` and the pickers accept: every github profile's login, then gh's other accounts.
func GitHubAccounts(reg *providers.Registry) []string {
	var out []string
	if reg != nil {
		for _, n := range reg.ProfilesOf("github") {
			if l := reg.Providers[n].Login; l != "" && !slices.Contains(out, l) {
				out = append(out, l)
			}
		}
	}
	for _, l := range copilot.GHAccounts() {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// githubProfile is the profile behind a GitHub account, written on first use: personal while it has no login,
// else a profile named after the account.
func githubProfile(reg *providers.Registry, login string) (string, error) {
	if reg != nil {
		for _, n := range reg.ProfilesOf("github") {
			if reg.Providers[n].Login == login {
				return n, nil
			}
		}
		if p := reg.Providers["personal"]; p != nil && p.Provider == "github" && p.Login == "" {
			return "personal", providers.SetProviderFields("personal", map[string]any{"login": login})
		}
	}
	return login, providers.AddGithubAccount(login)
}

// applyGitHubAccount pins login's profile (creating it on first use) as GitHub auth.
func applyGitHubAccount(d *dialog, login string) bool {
	_, known := d.reg.Providers[providers.GithubAccountName(d.reg, login)]
	profile, err := githubProfile(d.reg, login)
	if err != nil {
		d.printf("github auth: %v\n", err)
		return false
	}
	if !known {
		d.reload()
	}
	return applyGitHubAuth(d, profile)
}

// githubLogin is profile name's login ("" for the follow-gh pin); false, after a message, when the registry lacks it.
func githubLogin(d *dialog, name string) (string, bool) {
	if name == "" {
		return "", true
	}
	p := d.reg.Providers[name]
	if p == nil {
		d.printf("github auth: no profile %q in the registry; run configure github to add it\n", name)
		return "", false
	}
	return p.Login, true
}

// applyGitHubAuth switches gh to name's login and stores the pin, rolling gh back if the vault write fails.
func applyGitHubAuth(d *dialog, name string) bool {
	login, ok := githubLogin(d, name)
	if !ok {
		return false
	}
	prev := copilot.GHActiveAccount()
	if name != "" {
		if err := copilot.SwitchGH(login); err != nil {
			d.printf("github auth: %v\n", err)
			return false
		}
	}
	if err := vault.SetGitHubAuth(name); err != nil {
		d.printf("github auth: %v\n", err)
		if name != "" && prev != "" && prev != login {
			if rerr := copilot.SwitchGH(prev); rerr != nil { // roll gh back so it matches the stale pin
				d.printf("github auth: rollback to %s failed: %v\n", prev, rerr)
			}
		}
		return false
	}
	d.changed = true
	d.printf("github auth: %s\n", cmp.Or(login, "follows gh's active account"))
	return true
}

// runGithubAuthFlag is `configure github --auth ACCOUNT`: "" or "follow" clears the pin; --check verifies, --dry-run is a no-op.
func runGithubAuthFlag(d *dialog) bool {
	name := d.o.Auth
	valid := GitHubAccounts(d.reg)
	if name == "follow" && !slices.Contains(valid, name) { // a real account named "follow" wins over the keyword
		name = ""
	}
	if name != "" && !slices.Contains(valid, name) {
		d.printf("github auth: %q is not a known GitHub account; valid: follow, %s\n", name, strings.Join(valid, ", "))
		return false
	}
	if d.o.Check { // verification: pass only when the pinned account already equals name
		cur := providers.GithubAccountLogin(d.reg, vault.GitHubAuth())
		d.printf("github auth: pin is %q, want %q\n", cur, name)
		return cur == name
	}
	if d.o.DryRun {
		d.printf("github auth: would set %s\n", cmp.Or(name, "follow gh's active account"))
		return true
	}
	if name == "" {
		return applyGitHubAuth(d, "")
	}
	return applyGitHubAccount(d, name)
}

// runGithub is the `configure github` / `configure` "github" section: opencode's GitHub Copilot
// fallback ladder (not the copilot agent's own model, pinned on its own page via COPILOT_MODEL) —
// gh/copilot/opencode login state, the opencode and copilot routes, and — on a terminal — a menu to
// fix any of it or pick the github-copilot provider's opus/sonnet/haiku aliases.
func runGithub(d *dialog) bool {
	if d.o.AuthSet {
		return runGithubAuthFlag(d)
	}
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

// githubMenu loops a status report + action picker until Back/Esc; each action re-checks live state
// before the next render, so a login that just succeeded shows "present" immediately.
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
			{Name: "GitHub auth", Detail: "gh's active account: pick a profile or follow gh",
				Run: func() bool { pickGitHubAuth(d); return false }},
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

// pickCopilotModels lists the live copilot catalog (plus the synthetic "auto" router alias), asks for an
// id per opus/sonnet/haiku alias — the github-copilot provider's table opencode's fallback ladder reads,
// not the copilot agent's own model (COPILOT_MODEL, picked on the copilot agent page) — and, only if a
// alias resolved to "auto", a concrete opencode_fallback (opencode's own catalog has no "auto"), then
// writes the github-copilot overlay.
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
