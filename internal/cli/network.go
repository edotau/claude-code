package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent/tree"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/network/mcp"
	"github.com/edotau/claude-code/internal/network/web"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/ui/helptext"
	uitree "github.com/edotau/claude-code/internal/ui/tree"
)

var networkPage = helptext.Page{
	Usage: "claude-code network <command> [flags]",
	About: "Everything that reaches past this machine: the tailnet, browsers to automate,\n" +
		"web search, MCP server health, and the agent page served over the tailnet.",
	Commands: [][2]string{
		{"tailscale", "tailnet status; --machines --connect --key --exit-node --route"},
		{"browser", "list automatable browsers; --setup wires one up"},
		{"websearch", "stdio MCP web-search server; --query runs one search"},
		{"mcp", "MCP server health and inline secrets"},
		{"page", "serve the tailnet agent page and dashboard"},
		{"view", "print one agent's transcript; --follow streams it"},
		{"vscode", "VS Code web UI on the tailnet; --stop --status --funnel"},
		{"restart", "restart running daemons on the installed binary"},
		{"remote", "run an agent on a tailnet host in tmux"},
	},
	Hint: "claude-code network <command> -h lists that command's flags.",
}

// cmdNetwork is the one connectivity verb: tailnet, browsers, web search, MCP health and the agent page.
func cmdNetwork(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		networkPage.Fprint(os.Stderr)
		return 0
	}
	rest := args[1:]
	switch args[0] {
	case "tailscale":
		return cmdNetworkTailscale(rest)
	case "browser":
		return cmdNetworkBrowser(rest)
	case "websearch":
		return cmdNetworkWebsearch(rest)
	case "mcp":
		return cmdNetworkMCP(rest)
	case "page":
		return cmdNetworkPage(rest)
	case "view":
		return cmdNetworkView(rest)
	case "remote":
		return cmdNetworkRemote(rest)
	case "restart":
		return cmdRestart(args[1:])
	case "vscode":
		return cmdNetworkVSCode(rest)
	}
	names := make([]string, len(networkPage.Commands))
	for i, c := range networkPage.Commands {
		names[i] = c[0]
	}
	return fail("network: unknown subcommand %q (%s)", args[0], strings.Join(names, "|"))
}

// cmdNetworkMCP reads every MCP scope, runs live health, and flags drift and plaintext secrets; exit 1 on any.
func cmdNetworkMCP(args []string) int {
	fs := helptext.NewFlagSet("network mcp", helptext.Page{Usage: "claude-code network mcp [flags]",
		About: "Live health of every MCP server, plus cross-scope drift and inline plaintext secrets.\n" +
			"Read-only; exits 1 on any failed server or secret."})
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat("network mcp", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail("network mcp: %v", err)
	}
	claude, _ := claude.ClaudeBinary() // "" on error: Doctor fails OPEN on a missing claude binary
	cwd, _ := os.Getwd()
	settingsFiles := []string{
		filepath.Join(paths.ConfigDir(), "settings.json"),
		filepath.Join(paths.ConfigDir(), "settings.local.json"),
	}
	return mcp.Doctor(os.Stdout, mcp.Options{
		UserPath:      paths.ClaudeJSON(),
		ProjectFiles:  []string{filepath.Join(cwd, ".mcp.json")},
		SettingsFiles: settingsFiles,
		Claude:        claude,
		Home:          home,
		JSON:          *format == helptext.JSON,
	})
}

// cmdNetworkWebsearch runs the stdio MCP server when bare, or a one-shot Tavily print for a query or --query.
func cmdNetworkWebsearch(args []string) int {
	fs := helptext.NewFlagSet("network websearch", helptext.Page{Usage: "claude-code network websearch [flags] [<query>]",
		About: "Bare: the stdio MCP web-search server Claude Code runs. A query (or --query)\n" +
			"prints one Tavily search and exits."})
	query := fs.String("query", "", "run one search and exit (skip the MCP server loop)")
	maxResults := fs.Int("max-results", 5, "max results in one-shot mode")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		if *query != "" {
			fmt.Fprintln(os.Stderr, "network websearch: give the query either positionally or via --query, not both")
			return 2
		}
		*query = strings.Join(fs.Args(), " ")
	}
	if *query != "" {
		return mcp.OneShot(context.Background(), *query, *maxResults, nil, os.Stdout)
	}
	return mcp.Run(context.Background())
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
		return badFormat("restart", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	row := proxy.RestartReport(ctx, *dry, *force)
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, map[string]any{"daemons": []proxy.RestartRow{row}}); err != nil {
			return fail("restart: %v", err)
		}
	} else {
		printRestartRow(row)
	}
	if row.Status == "failed" {
		return 1
	}
	return 0
}

func printRestartRow(r proxy.RestartRow) {
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

var networkViewPage = helptext.Page{Usage: "claude-code network view [flags] <agentId|prefix>",
	About: "Print one subagent's transcript snapshot; --follow then streams its live events."}

// cmdNetworkView is the drill-down into one subagent: a snapshot, then with --follow its live event stream.
func cmdNetworkView(args []string) int {
	fs := helptext.NewFlagSet("network view", networkViewPage)
	session := fs.String("session", "", "session id to search (default: every session in --days)")
	days := fs.Int("days", 1, "session scope in days")
	n := fs.Int("n", 40, "trailing events in the snapshot")
	follow := fs.Bool("follow", false, "after the snapshot, stream new events until the agent finishes")
	format := helptext.FormatFlag(fs)
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat("network view", err)
	}
	if len(rest) != 1 {
		fs.Usage()
		return 2
	}
	opts := tree.ViewOptions{ClaudeDir: paths.ConfigDir(), Session: *session, Days: *days, Agent: rest[0], N: *n}
	snap, err := tree.View(opts)
	if err != nil {
		return fail("network view: %v", err)
	}
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, snap); err != nil {
			return fail("network view: %v", err)
		}
	} else {
		uitree.RenderSnapshot(os.Stdout, snap)
	}
	if !*follow {
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := uitree.Follow(ctx, opts, os.Stdout); err != nil && ctx.Err() == nil {
		return fail("network view: %v", err)
	}
	return 0
}

func cmdNetworkPage(args []string) int {
	fs := helptext.NewFlagSet("network page", helptext.Page{Usage: "claude-code network page [flags]",
		About: "Serve the agent page and dashboard on the tailnet."})
	addr := fs.String("addr", "", "bind address (default: this node's tailnet IPv4 on port 7777)")
	hosts := fs.String("hosts", "", "comma-separated dispatch hosts (default: tailnet machines)")
	days := fs.Int("days", 1, "agent-tree scope in days")
	dryRun := fs.Bool("dry-run-dispatch", false, "POST /api/dispatch returns the ssh argv instead of running it")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	var hostList []string
	if *hosts != "" {
		hostList = strings.Split(*hosts, ",")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := web.Run(ctx, web.Options{
		Addr: *addr, Hosts: hostList, Days: *days, DryRunDispatch: *dryRun,
		ErrOut: os.Stderr, Verbs: func() any { return verbTree() },
	}); err != nil {
		return fail("network page: %v", err)
	}
	return 0
}
