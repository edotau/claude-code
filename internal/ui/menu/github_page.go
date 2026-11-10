package menu

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	uidialog "github.com/edotau/claude-code/internal/ui/dialog"
	"github.com/edotau/claude-code/internal/vault"
)

// printRoute prints one agent.DescribeRoute result: provider/model/small_model and why, never a credential.
func printRoute(d *dialog, ri agent.RouteInfo) {
	status := "FIX"
	if ri.OK {
		status = "ok"
	}
	d.printf("%-9s route(%-8s) %-4s provider=%-16s model=%-24s small_model=%-20s %s\n",
		ri.Agent, ri.Route, status, ri.Provider, ri.Model, ri.SmallModel, ri.Reason)
}

// githubStatusRows prints every github section status line and reports which are OK, for both the
// interactive menu (re-run each loop) and the piped report (run once).
func githubStatusRows(d *dialog) (ghOK, copilotOK bool) {
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

	oc, _ := agent.LookupSchema("opencode")
	cp, _ := agent.LookupSchema("copilot")
	printRoute(d, oc.DescribeRoute(d.reg))
	printRoute(d, cp.DescribeRoute(d.reg))
	return
}

// githubProfile is the named github profile, or nil when absent or of another provider.
func (d *dialog) githubProfile(name string) *providers.Provider {
	if d.reg == nil {
		return nil
	}
	if p := d.reg.Providers[name]; p != nil && p.Provider == "github" {
		return p
	}
	return nil
}

// githubAuthLine is the GitHub auth pin and whether gh's active account matches it.
func githubAuthLine(d *dialog) string {
	profile, login := providers.GitHubAuthLogin(d.reg)
	if profile == "" {
		profile = vault.GitHubAuth()
	}
	if profile == "" {
		return "follows gh's active account"
	}
	if login == "" {
		if p := d.githubProfile(profile); p == nil {
			return profile + " · drift: pin names no github profile, gh follows its active account"
		}
		return profile + " · drift: profile has no login, gh follows its active account"
	}
	if active := copilot.GHActiveAccount(); active != login {
		return profile + " (" + login + ") · drift: gh active is " + cmp.Or(active, "none")
	}
	return profile + " (" + login + ")"
}

// pickGitHubAuth lists github profiles with a login plus "follow gh active"; a pick switches gh, then stores it.
func pickGitHubAuth(d *dialog) {
	names := []string{""}
	rows := [][2]string{{"follow gh active", "no switch; use whichever account gh has active"}}
	for _, n := range d.reg.ProfilesOf("github") {
		if l := d.reg.Providers[n].Login; l != "" {
			names = append(names, n)
			rows = append(rows, [2]string{n, "gh account " + l})
		}
	}
	idx, err := d.shell().Pick("GitHub auth — gh's active account", rows)
	if err != nil || idx < 0 {
		return
	}
	prev := copilot.GHActiveAccount()
	if names[idx] != "" {
		if err := copilot.SwitchGH(d.reg.Providers[names[idx]].Login); err != nil {
			d.printf("github auth: %v\n", err)
			return
		}
	}
	if err := vault.SetGitHubAuth(names[idx]); err != nil {
		d.printf("github auth: %v\n", err)
		if names[idx] != "" && prev != "" && prev != d.reg.Providers[names[idx]].Login {
			if rerr := copilot.SwitchGH(prev); rerr != nil { // roll gh back so it matches the stale pin
				d.printf("github auth: rollback to %s failed: %v\n", prev, rerr)
			}
		}
		return
	}
	d.changed = true
	d.printf("github auth: %s\n", cmp.Or(names[idx], "follows gh's active account"))
}

// runGithub is the `configure github` / `configure` "github" section: opencode's GitHub Copilot
// fallback ladder (not the copilot agent's own model, pinned on its own page via COPILOT_MODEL) —
// gh/copilot/opencode login state, the opencode and copilot routes, and — on a terminal — a menu to
// fix any of it or pick the github-copilot provider's opus/sonnet/haiku aliases.
func runGithub(d *dialog) bool {
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		return githubMenu(d)
	}
	ghOK, copilotOK := githubStatusRows(d)
	if !ghOK {
		d.println("fix: gh auth login")
	}
	if !copilotOK {
		d.println("fix: copilot login")
	}
	if d.o.Check {
		oc, _ := agent.LookupSchema("opencode")
		cp, _ := agent.LookupSchema("copilot")
		return oc.DescribeRoute(d.reg).OK && cp.DescribeRoute(d.reg).OK
	}
	return true
}

// githubMenu loops a status report + action picker until Back/Esc; each action re-checks live state
// before the next render, so a login that just succeeded shows "present" immediately.
func githubMenu(d *dialog) bool {
	login := func(name, detail, label string, argv ...string) uidialog.Row {
		return uidialog.Row{Name: name, Detail: detail, Run: func() bool { runGithubLogin(d, argv, label); return false }}
	}
	d.shell().Run("GitHub (opencode fallback) — pick an action", func() []uidialog.Row {
		githubStatusRows(d)
		return []uidialog.Row{
			login("Log in to GitHub CLI", "gh auth login", "gh CLI", "gh", "auth", "login"),
			login("Log in to Copilot", "copilot login", "copilot", "copilot", "login"),
			login("Log in to OpenCode", "opencode auth login (choose GitHub Copilot)", "opencode", "opencode", "auth", "login"),
			{Name: "GitHub auth", Detail: "gh's active account: pick a profile or follow gh",
				Run: func() bool { pickGitHubAuth(d); return false }},
			{Name: "Pick fallback models", Detail: "opus/sonnet/haiku github-copilot serves to opencode",
				Run: func() bool { pickCopilotModels(d); return false }},
		}
	})
	return true
}

// runGithubLogin execs argv attached to the real terminal, same contract as runLogin elsewhere in configure.
func runGithubLogin(d *dialog, argv []string, label string) {
	if err := configure.RunLogin(argv); err != nil {
		d.printf("%s: %v\n", label, err)
		return
	}
	d.printf("%s: login flow finished\n", label)
}

// pickCopilotModels lists the live copilot catalog (plus the synthetic "auto" router alias), asks for an
// id per opus/sonnet/haiku alias — the github-copilot provider's table opencode's fallback ladder reads,
// not the copilot agent's own model (COPILOT_MODEL, picked on the copilot agent page) — and, only if a
// alias resolved to "auto", a concrete opencode_fallback (opencode's own catalog has no "auto"), then
// writes the github-copilot overlay.
func pickCopilotModels(d *dialog) {
	list, ok := copilotCatalogModels(d)
	if !ok {
		return
	}
	ids := []string{"auto"}
	rows := [][2]string{{"auto", "proxy alias — resolve automatically (needs opencode_fallback for opencode)"}}
	for _, m := range list {
		ids = append(ids, m.ID)
		rows = append(rows, [2]string{m.ID, m.Name})
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

// copilotCatalogModels lists the live copilot catalog, printing the reason and returning false when it is unavailable.
func copilotCatalogModels(d *dialog) (list []copilot.SDKModel, ok bool) {
	bin, err := paths.AgentBin("copilot")
	if err != nil {
		d.printf("copilot models: copilot CLI not found: %v\n", err)
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opts := copilot.CatalogOptions(bin, copilot.CopilotToken())
	tok, err := copilot.CopilotLaunchToken()
	if err != nil {
		d.printf("copilot models: %v\n", err)
		return nil, false
	}
	if tok != "" {
		opts.GitHubToken, opts.UseLoggedInUser = tok, false
	}
	list, err = copilot.SDKListModels(ctx, opts)
	if err != nil {
		d.printf("copilot models: %v\n", err)
		return nil, false
	}
	return list, true
}
