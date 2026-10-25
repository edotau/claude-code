package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/websearch"
)

// cmdNetworkWebsearch: bare `network websearch` runs the stdio MCP server (Claude Code invokes this); `network websearch <query>`
// or --query is a one-shot Tavily print for Bash/skill use. Both Tavily-backed (internal/websearch).
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
		return websearch.OneShot(context.Background(), *query, *maxResults, nil, os.Stdout)
	}
	return websearch.Run(context.Background())
}
