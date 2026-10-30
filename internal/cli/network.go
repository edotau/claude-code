package cli

import (
	"os"

	"github.com/edotau/claude-code/internal/helptext"
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
	return fail("network: unknown subcommand %q (tailscale|browser|websearch|mcp|page|view|vscode|restart|remote)", args[0])
}
