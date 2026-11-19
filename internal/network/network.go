package network

import (
	"os"
	"strings"

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
		{"tailscale", "tailnet status; --machines --connect --token --exit-node/subnet"},
		{"browser", "list automatable browsers; --setup wires one up"},
		{"websearch", "stdio MCP web-search server; --query runs one search"},
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

// Spec is the completion node for the network verb, derived from each command's FlagSet.
func Spec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"tailscale": {Flags: tailscale.Flags()},
		"browser":   {Flags: browser.Flags()},
		"websearch": {Flags: websearch.Flags()},
		"remote":    {Flags: remote.Flags()},
	}}
}
