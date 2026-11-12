package network

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/network/browser"
	"github.com/edotau/claude-code/internal/network/egress"
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
		{"tailscale", "tailnet status; --machines --connect --key --exit-node/subnet/route"},
		{"browser", "list automatable browsers; --setup wires one up"},
		{"websearch", "stdio MCP web-search server; --query runs one search"},
		{"restart", "restart the loopback proxy on the installed binary"},
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
	About: "Restart the loopback proxy on the installed binary; a stopped proxy is\n" +
		"skipped. The memory idle watcher is per-session and exits by itself, so\n" +
		"it is not restarted.\n" +
		"A proxy with requests in flight is skipped, exit 0, unless --force;\n" +
		"open sessions alone do not block a restart.\n" +
		"Each --remote ssh tunnel is stopped too, unless the proxy was left busy;\n" +
		"the next --remote command starts it again.",
}

// restartFlags declares `network restart`'s flags on fs.
func restartFlags(fs *flag.FlagSet) (dry, force *bool, format *string) {
	dry = fs.Bool("dry-run", false, "report what would restart; change nothing")
	force = fs.Bool("force", false, "restart the proxy despite requests in flight")
	format = helptext.FormatFlag(fs)
	return
}

func cmdRestart(args []string) int {
	fs := helptext.NewFlagSet("network restart", restartPage)
	dry, force, format := restartFlags(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("network restart", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	row := gateway.RestartReport(ctx, *dry, *force)
	rows := []gateway.RestartRow{row}
	if busy := row.Status == "skipped" && row.OldPID != 0; !busy {
		rows = append(rows, stopTunnels(*dry)...)
	}
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, map[string]any{"daemons": rows}); err != nil {
			return helptext.Fail("network restart: %v", err)
		}
	}
	code := 0
	for _, r := range rows {
		if *format != helptext.JSON {
			printRestartRow(r)
		}
		if r.Status == "failed" {
			code = 1
		}
	}
	return code
}

// stopTunnels ends every recorded --remote ssh tunnel (dry: reports them), one row per egress host.
func stopTunnels(dry bool) []gateway.RestartRow {
	var rows []gateway.RestartRow
	for _, t := range egress.Tunnels() {
		row := gateway.RestartRow{Daemon: "egress", Status: "stopped", OldPID: t.PID, Note: t.Host}
		if dry {
			row.Status = "would stop"
		} else if err := egress.Stop(t.Host); err != nil {
			row.Status, row.Note = "failed", err.Error()
		}
		rows = append(rows, row)
	}
	return rows
}

func printRestartRow(r gateway.RestartRow) {
	switch r.Status {
	case "restarted":
		fmt.Printf("%-8s restarted  pid %d → %d\n", r.Daemon, r.OldPID, r.NewPID)
	case "would restart":
		fmt.Printf("%-8s would restart  pid %d\n", r.Daemon, r.OldPID)
	case "stopped", "would stop":
		fmt.Printf("%-8s %s  %s pid %d\n", r.Daemon, r.Status, r.Note, r.OldPID)
	case "skipped":
		fmt.Printf("%-8s %s (skipped)\n", r.Daemon, r.Note)
	default:
		fmt.Fprintf(os.Stderr, "%-8s restart failed: %s\n", r.Daemon, r.Note)
	}
}

func restartSpecFlags() []helptext.Flag {
	fs := helptext.NewFlagSet("network restart", helptext.Page{})
	restartFlags(fs)
	return helptext.FlagsOf(fs, nil)
}

// Spec is the completion node for the network verb, derived from each command's FlagSet.
func Spec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"restart":   {Flags: restartSpecFlags()},
		"tailscale": {Flags: tailscale.Flags()},
		"browser":   {Flags: browser.Flags()},
		"websearch": {Flags: websearch.Flags()},
		"remote":    {Flags: remote.Flags()},
	}}
}
