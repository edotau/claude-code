package configure

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// ghAccountRE pulls the account name out of `gh auth status`'s "Logged in to github.com account <name>" line.
var ghAccountRE = regexp.MustCompile(`account\s+(\S+)`)

// ghStatus is what `gh auth status` reports: whether the binary exists, whether it is logged in, and the
// account name — never a token (gh itself only ever prints a masked one).
type ghStatus struct {
	found    bool
	loggedIn bool
	account  string
}

// ghAuthCheck runs `gh auth status` (8s cap); its exit code is the verdict, its stdout+stderr the account line.
func ghAuthCheck() ghStatus {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return ghStatus{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "auth", "status").CombinedOutput()
	st := ghStatus{found: true, loggedIn: err == nil}
	if m := ghAccountRE.FindStringSubmatch(string(out)); len(m) > 1 {
		st.account = m[1]
	}
	return st
}

// printRoute prints one agent.DescribeRoute result: provider/model/small_model and why, never a credential.
func printRoute(d *dialog, ri agent.RouteInfo) {
	status := "FIX"
	if ri.OK {
		status = "ok"
	}
	d.printf("%-9s route(%-8s) %-4s provider=%-16s model=%-24s small_model=%-20s %s\n",
		ri.Agent, ri.Leg, status, ri.Provider, ri.Model, ri.SmallModel, ri.Reason)
}

// githubStatusRows prints every github section status line and reports which are OK, for both the
// interactive menu (re-run each loop) and the piped report (run once).
func githubStatusRows(d *dialog) (ghOK, copilotOK bool) {
	gh := ghAuthCheck()
	switch {
	case !gh.found:
		d.printf("%-16s ABSENT: gh CLI not found on PATH\n", "gh CLI")
	case gh.loggedIn:
		acct := agent.GHActiveAccount()
		if acct == "" {
			acct = gh.account
		}
		d.printf("%-16s present (active account %s)\n", "gh CLI", acct)
		ghOK = true
	default:
		d.printf("%-16s ABSENT: not logged in\n", "gh CLI")
	}

	auth, copilotOK := agent.ResolveCopilotAuth()
	if copilotOK {
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

// runGithub is the `configure github` / `configure` "github" section: opencode's GitHub Copilot
// fallback ladder (not the copilot agent's own model, pinned on its own page via COPILOT_MODEL) —
// gh/copilot/opencode login state, the opencode and copilot routes, and — on a terminal — a menu to
// fix any of it or pick the github-copilot provider's opus/sonnet/haiku slots.
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
	for {
		githubStatusRows(d)
		rows := [][2]string{
			{"Log in to GitHub CLI", "gh auth login"},
			{"Log in to Copilot", "copilot login"},
			{"Log in to OpenCode", "opencode auth login (choose GitHub Copilot)"},
			{"Pick fallback models", "opus/sonnet/haiku github-copilot serves to opencode"},
		}
		idx, err := tui.PickBack("GitHub (opencode fallback) — pick an action", rows, 0)
		if err != nil || idx < 0 {
			return true
		}
		switch idx {
		case 0:
			runGithubLogin(d, []string{"gh", "auth", "login"}, "gh CLI")
		case 1:
			runGithubLogin(d, []string{"copilot", "login"}, "copilot")
		case 2:
			runGithubLogin(d, []string{"opencode", "auth", "login"}, "opencode")
		case 3:
			pickCopilotModels(d)
		}
	}
}

// runGithubLogin execs argv attached to the real terminal, same contract as runLogin elsewhere in configure.
func runGithubLogin(d *dialog, argv []string, label string) {
	if err := runLogin(argv); err != nil {
		d.printf("%s: %v\n", label, err)
		return
	}
	d.printf("%s: login flow finished\n", label)
}

// pickCopilotModels lists the live copilot catalog (plus the synthetic "auto" router alias), asks for an
// id per opus/sonnet/haiku slot — the github-copilot provider's table opencode's fallback ladder reads,
// not the copilot agent's own model (COPILOT_MODEL, picked on the copilot agent page) — and, only if a
// slot resolved to "auto", a concrete opencode_fallback (opencode's own catalog has no "auto"), then
// writes the github-copilot overlay.
func pickCopilotModels(d *dialog) {
	bin, err := paths.VendorBin("copilot")
	if err != nil {
		d.printf("copilot models: copilot CLI not found: %v\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opts := copilotCatalogOptions(bin)
	tok, err := agent.CopilotLaunchToken()
	if err != nil {
		d.printf("copilot models: %v\n", err)
		return
	}
	if tok != "" {
		opts.GitHubToken, opts.UseLoggedInUser = tok, false
	}
	list, err := copilotsdk.ListModels(ctx, opts)
	if err != nil {
		d.printf("copilot models: %v\n", err)
		return
	}

	ids := []string{"auto"}
	rows := [][2]string{{"auto", "proxy alias — resolve automatically (needs opencode_fallback for opencode)"}}
	for _, m := range list {
		ids = append(ids, m.ID)
		rows = append(rows, [2]string{m.ID, m.Name})
	}

	picks := map[string]string{}
	for _, slot := range []string{models.Opus, models.Sonnet, models.Haiku} {
		idx, err := tui.PickBack("github fallback — "+slot+" model", rows, 0)
		if err != nil || idx < 0 {
			d.printf("github fallback: cancelled at the %s model; nothing written\n", slot)
			return
		}
		picks[slot] = ids[idx]
	}

	fallback := ""
	needsFallback := false
	for _, id := range picks {
		if id == "auto" {
			needsFallback = true
		}
	}
	if needsFallback {
		fbIDs := make([]string, 0, len(list))
		fbRows := make([][2]string, 0, len(list))
		for _, m := range list {
			fbIDs = append(fbIDs, m.ID)
			fbRows = append(fbRows, [2]string{m.ID, m.Name})
		}
		idx, err := tui.PickBack("github fallback — opencode_fallback (auto needs a concrete id)", fbRows, 0)
		if err != nil || idx < 0 {
			d.printf("github fallback: cancelled at opencode_fallback; nothing written\n")
			return
		}
		fallback = fbIDs[idx]
	}

	if err := WriteGithubCopilotModels(picks, fallback); err != nil {
		d.printf("github fallback: %v\n", err)
		return
	}
	suffix := ""
	if fallback != "" {
		suffix = " opencode_fallback=" + fallback
	}
	d.printf("github fallback: wrote vault overlay/providers (opus=%s sonnet=%s haiku=%s%s)\n",
		picks[models.Opus], picks[models.Sonnet], picks[models.Haiku], suffix)
}

// WriteGithubCopilotModels validates and merges picks and fallback into vault overlay/providers.
func WriteGithubCopilotModels(picks map[string]string, fallback string) error {
	for _, slot := range []string{models.Opus, models.Sonnet, models.Haiku} {
		if strings.TrimSpace(picks[slot]) == "" {
			return fmt.Errorf("github-copilot models: %s model is empty", slot)
		}
	}
	if fallback == "auto" {
		return errors.New(`github-copilot opencode_fallback: "auto" is not a valid fallback (opencode's own catalog has no alias by that name)`)
	}
	fields := map[string]any{"models": picks}
	if fallback != "" {
		fields["opencode_fallback"] = fallback
	}
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	p := profileBy(reg, "github", providers.AuthCLI)
	if p == nil {
		return errors.New("github models: no github profile in the registry")
	}
	return providers.SetProviderFields(p.Name, fields)
}

// copilotCatalogOptions lists models as the account copilot runs as (COPILOT_PROFILE), not its stored login.
func copilotCatalogOptions(bin string) copilotsdk.Options {
	return copilotsdk.CatalogOptions(bin, agent.CopilotToken())
}
