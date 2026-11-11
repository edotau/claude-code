package network

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/network/browser"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/network/websearch"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var networkPage = helptext.Page{
	Usage: "claude-code network <command> [flags]",
	About: "Everything that reaches past this machine: the tailnet, browsers to automate,\n" +
		"web search and agents on tailnet hosts.",
	Commands: [][2]string{
		{"tailscale", "tailnet status; --machines --connect --key --exit-node --route"},
		{"browser", "list automatable browsers; --setup wires one up"},
		{"websearch", "stdio MCP web-search server; --query runs one search"},
		{"restart", "restart running daemons on the installed binary"},
		{"remote", "run an agent on a tailnet host in tmux"},
	},
	Hint: "claude-code network <command> -h lists that command's flags.",
}

// Run is the one connectivity verb: tailnet, browsers, web search and remote agents.
func Run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		networkPage.Fprint(os.Stderr)
		return 0
	}
	handlers := map[string]func([]string) int{
		"tailscale": tailscale.Cmd,
		"browser":   browser.Cmd,
		"websearch": websearch.Cmd,
		"remote":    remote.Cmd,
		"restart":   cmdRestart,
	}
	if handler, ok := handlers[args[0]]; ok {
		return handler(args[1:])
	}
	names := make([]string, len(networkPage.Commands))
	for i, c := range networkPage.Commands {
		names[i] = c[0]
	}
	return helptext.Fail("network: unknown subcommand %q (%s)", args[0], strings.Join(names, "|"))
}

var restartPage = helptext.Page{
	Usage: "claude-code network restart [--dry-run] [--force] [--format text|json]",
	About: "Restart each running harness daemon on the installed binary. Stopped\n" +
		"daemons are skipped. Covers the proxy; the memory idle watcher is\n" +
		"per-session and exits by itself, so it is not restarted.\n" +
		"A proxy with requests in flight is skipped, exit 0, unless --force;\n" +
		"open sessions alone do not block a restart.",
}

func cmdRestart(args []string) int {
	fs := helptext.NewFlagSet("network restart", restartPage)
	dry := fs.Bool("dry-run", false, "report what would restart; change nothing")
	force := fs.Bool("force", false, "restart the proxy despite requests in flight")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("network restart", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	row := gateway.RestartReport(ctx, *dry, *force)
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, map[string]any{"daemons": []gateway.RestartRow{row}}); err != nil {
			return helptext.Fail("network restart: %v", err)
		}
	} else {
		printRestartRow(row)
	}
	if row.Status == "failed" {
		return 1
	}
	return 0
}

func printRestartRow(r gateway.RestartRow) {
	switch r.Status {
	case "restarted":
		fmt.Printf("%-8s restarted  pid %d → %d\n", r.Daemon, r.OldPID, r.NewPID)
	case "would restart":
		fmt.Printf("%-8s would restart  pid %d\n", r.Daemon, r.OldPID)
	case "skipped":
		fmt.Printf("%-8s %s (skipped)\n", r.Daemon, r.Note)
	default:
		fmt.Fprintf(os.Stderr, "%-8s restart failed: %s\n", r.Daemon, r.Note)
	}
}

// Spec is the completion node for the network verb.
func Spec() *helptext.Node {
	flag, boolFlag := helptext.Plain, helptext.BoolFlag
	valuedFlag, formatFlag := helptext.ValuedFlag, helptext.CompletionFormatFlag
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"restart": {Flags: []helptext.Flag{boolFlag("dry-run"), boolFlag("force"), formatFlag()}},
		"tailscale": {Flags: []helptext.Flag{formatFlag(), boolFlag("machines"), boolFlag("connect"), boolFlag("key"),
			valuedFlag("exit-node", exitNodeActions), valuedFlag("subnet", subnetActions),
			valuedFlag("route", routeTargets), flag("auth-key"),
			flag("key-env"), boolFlag("advertise-exit"),
			flag("expiry"), flag("tags"), boolFlag("save"), boolFlag("reusable"), boolFlag("ephemeral"),
			boolFlag("preauthorized"), flag("description")}},
		"browser": {Flags: []helptext.Flag{formatFlag(), boolFlag("setup"), valuedFlag("browser", browserNames),
			boolFlag("dry-run")}},
		"websearch": {Flags: []helptext.Flag{flag("query"), flag("max-results"), flag("extract")}},
		"remote": {Flags: []helptext.Flag{valuedFlag("host", tailscale.CachedMachineNames), flag("dir"),
			valuedFlag("agent", func() []string { return remote.Agents }), flag("name"), boolFlag("dry-run"),
			boolFlag("list"), boolFlag("attach"), boolFlag("status"), flag("interval"), formatFlag()}},
	}}
}

// exitNodeActions are the `network tailscale --exit-node` values.
func exitNodeActions() []string { return []string{"on", "off", "approve"} }

// subnetActions are the `network tailscale --subnet` keyword values (a CIDR list is also accepted).
func subnetActions() []string { return []string{"approve", "off"} }

// routeTargets are the `network tailscale --route` values: dispatch hosts or "off" to clear.
func routeTargets() []string { return append(tailscale.CachedMachineNames(), "off") }

// browserNames mirrors `network browser --setup --browser`'s accepted values.
func browserNames() []string {
	return []string{"chrome", "edge", "brave", "arc", "vivaldi", "chromium", "default"}
}
