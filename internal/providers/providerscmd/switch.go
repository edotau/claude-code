package providerscmd

import (
	"context"
	"fmt"
	"maps"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

var switchPage = helptext.Page{
	Usage: "claude-code switch <profile> [flags]",
	About: "Sets the profile on every agent that can serve it; relaunch to apply. One launch only:\n" +
		"claude-code --profile <p>.",
	Hint: "claude-code configure provider --list shows each profile's credential state.",
}

// Switch is the switch verb: it pins the session profile on every agent that can serve it.
func Switch(args []string) int {
	dryRun, args := helptext.ExtractBool(args, "dry-run")
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		switchUsage()
		return 0
	}
	if len(args) != 1 {
		switchUsage()
		return 2
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if _, err := reg.Get(args[0]); err != nil {
		return helptext.Fail("%v", err)
	}
	if dryRun {
		return previewSwitchProfile(reg, args[0])
	}
	if err := configure.SwitchProfile(reg, args[0]); err != nil {
		return helptext.Fail("%v", err)
	}
	fmt.Printf("profile pinned: %s (relaunch to apply)\n", args[0])
	preflight.Warm(os.Stdout, reg, time.Now())
	return 0
}

func previewSwitchProfile(reg *providers.Registry, profile string) int {
	apply := func(m map[string]vault.AgentRecord) { configure.SwitchRecords(reg, m, profile) }
	recs, err := vault.ReadAgentRecords(agent.RosterNames())
	if err != nil {
		return helptext.Fail("switch preview: %v", err)
	}
	return printRecordDiff(apply, keptOff(reg, recs, profile))
}

// switchUsage prints switch's page; helptext.ExtractBool pulls its one flag out, so this FlagSet only renders it.
func switchUsage() {
	fs := helptext.NewFlagSet("switch", switchPage)
	fs.Bool("dry-run", false, "print the agent record change, write nothing")
	fs.Usage()
}

var resolvePage = helptext.Page{
	Usage: "claude-code resolve [flags]",
	About: "Resolves and prints the session profile's credential: the apiKeyHelper\n" +
		"Claude Code calls. --proxy prints the loopback proxy's client secret instead.",
}

// Resolve is the apiKeyHelper: --proxy prints the loopback proxy's client secret instead.
func Resolve(args []string) int {
	fs := helptext.NewFlagSet("resolve", resolvePage)
	profile := fs.String("profile", "", "profile name")
	viaRouter := fs.Bool("proxy", false, "print the loopback proxy client secret")
	as := fs.String("as", "", "Databricks identity the launch carries: auto|user|sp|headless")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *viaRouter {
		s, err := proxy.ClientSecret()
		if err != nil {
			return helptext.Fail("%v", err)
		}
		fmt.Println(s)
		return 0
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("%v", err)
	}
	sel, err := providers.Select(reg, *profile)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	tok, err := providers.Credential(providers.WithIdentity(context.Background(), *as), sel.Provider)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	fmt.Fprintln(os.Stdout, tok)
	return 0
}

// keptOff lists "agent: off (kept)" for each off agent that could serve profile and a switch leaves off.
func keptOff(reg *providers.Registry, recs map[string]vault.AgentRecord, profile string) []string {
	_, off := configure.SwitchTargets(reg, recs, profile)
	lines := make([]string, len(off))
	for i, n := range off {
		lines[i] = n + ": off (kept)"
	}
	return lines
}

// printRecordDiff is the shared --dry-run body for switch and models.
func printRecordDiff(apply func(map[string]vault.AgentRecord), notes []string) int {
	before, err := vault.ReadAgentRecords(agent.RosterNames())
	if err != nil {
		return helptext.Fail("agent record preview: %v", err)
	}
	after := maps.Clone(before)
	apply(after)
	if err := printLines(providers.DryRunLines(before, after, notes)); err != nil {
		return helptext.Fail("agent record preview: %v", err)
	}
	return 0
}
