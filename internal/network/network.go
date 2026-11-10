package network

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/network/mcp"
	"github.com/edotau/claude-code/internal/proxy"
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
		"tailscale": cmdNetworkTailscale,
		"browser":   cmdNetworkBrowser,
		"websearch": cmdNetworkWebsearch,
		"remote":    cmdNetworkRemote,
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

// cmdNetworkWebsearch runs the stdio MCP server when bare, or a one-shot Tavily print for a query or --query.
func cmdNetworkWebsearch(args []string) int {
	fs := helptext.NewFlagSet("network websearch", helptext.Page{Usage: "claude-code network websearch [flags] [<query> | --extract <url>...]",
		About: "Bare: the stdio MCP web-search server Claude Code runs. A query (or --query)\n" +
			"prints one Tavily search and exits."})
	query := fs.String("query", "", "run one search and exit (skip the MCP server loop)")
	maxResults := fs.Int("max-results", 5, "max results in one-shot mode")
	var extract urlList
	fs.Var(&extract, "extract", "print the page at URL as markdown (repeatable)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if len(extract) > 0 {
		if *query != "" || fs.NArg() > 0 {
			fmt.Fprintln(os.Stderr, "network websearch: --extract cannot be combined with a query")
			return 2
		}
		return mcp.Extract(context.Background(), extract)
	}
	if err := joinPositionalQuery(fs, query); err != nil {
		fmt.Fprintln(os.Stderr, "network websearch:", err)
		return 2
	}
	if *query != "" {
		return mcp.OneShot(context.Background(), *query, *maxResults, nil, os.Stdout)
	}
	return mcp.Run(context.Background())
}

// joinPositionalQuery folds positional args into *query; giving both forms is an error.
func joinPositionalQuery(fs *flag.FlagSet, query *string) error {
	if fs.NArg() == 0 {
		return nil
	}
	if *query != "" {
		return errors.New("give the query either positionally or via --query, not both")
	}
	*query = strings.Join(fs.Args(), " ")
	return nil
}

// urlList collects repeatable --extract values.
type urlList []string

func (l *urlList) String() string { return strings.Join(*l, ",") }
func (l *urlList) Set(s string) error {
	*l = append(*l, s)
	return nil
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
		return helptext.BadFormat("restart", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	row := proxy.RestartReport(ctx, *dry, *force)
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, map[string]any{"daemons": []proxy.RestartRow{row}}); err != nil {
			return helptext.Fail("restart: %v", err)
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
