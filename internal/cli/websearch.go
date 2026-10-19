package cli

import (
	"context"
	"flag"
	"os"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/websearch"
)

// cmdWebSearch: bare `web-search` runs the stdio MCP server (Claude Code invokes this); --query is
// a one-shot Tavily print for Bash/skill use. Both Tavily-backed (internal/websearch).
func cmdWebSearch(args []string) int {
	fs := flag.NewFlagSet("web-search", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of web-search:\n")
		helptext.Defaults(fs)
	}
	query := fs.String("query", "", "run one search and exit (skip the MCP server loop)")
	maxResults := fs.Int("max-results", 5, "max results in one-shot mode")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *query != "" {
		return websearch.OneShot(context.Background(), *query, *maxResults, nil, os.Stdout)
	}
	return websearch.Run(context.Background())
}
