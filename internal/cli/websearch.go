package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/websearch"
)

// cmdNetworkWebsearch: bare `network websearch` runs the stdio MCP server (Claude Code invokes this); `network websearch <query>`
// or --query is a one-shot Tavily print for Bash/skill use. Both Tavily-backed (internal/websearch).
func cmdNetworkWebsearch(args []string) int {
	fs := flag.NewFlagSet("network websearch", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of network websearch [<query>]:\n")
		helptext.Defaults(fs)
	}
	query := fs.String("query", "", "run one search and exit (skip the MCP server loop)")
	maxResults := fs.Int("max-results", 5, "max results in one-shot mode")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		if *query != "" {
			fmt.Fprintln(os.Stderr, "network websearch: give the query either positionally or via --query, not both")
			return 2
		}
		*query = strings.Join(fs.Args(), " ")
	}
	if *query != "" {
		return websearch.OneShot(context.Background(), *query, *maxResults, nil, os.Stdout)
	}
	return websearch.Run(context.Background())
}
