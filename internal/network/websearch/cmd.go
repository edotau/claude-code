package websearch

import (
	"context"
	"errors"
	"flag"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/ui/helptext"
)

// searchFlags declares `network websearch`'s flags on fs.
func searchFlags(fs *flag.FlagSet) (query *string, maxResults *int, extract *urlList) {
	query = fs.String("query", "", "run one search and exit (skip the MCP server loop)")
	maxResults = fs.Int("max-results", 5, "max results in one-shot mode")
	extract = new(urlList)
	fs.Var(extract, "extract", "print the page at URL as markdown (repeatable)")
	return
}

// Flags are the completion flags of `network websearch`, derived from its FlagSet.
func Flags() []helptext.Flag {
	fs := helptext.NewFlagSet("network websearch", helptext.Page{})
	searchFlags(fs)
	return helptext.FlagsOf(fs, nil)
}

// Cmd runs the stdio MCP server when bare, or a one-shot Tavily print for a query or --query.
func Cmd(args []string) int {
	fs := helptext.NewFlagSet("network websearch", helptext.Page{
		Usage: "claude-code network websearch [flags] [<query> | --extract <url>...]",
		About: "Bare: the stdio MCP web-search server Claude Code runs. A query (or --query)\n" +
			"prints one Tavily search and exits."})
	query, maxResults, extract := searchFlags(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if len(*extract) > 0 {
		if *query != "" || fs.NArg() > 0 {
			return helptext.FailUsage("network websearch: --extract cannot be combined with a query")
		}
		return extractURLs(context.Background(), *extract)
	}
	if err := joinPositionalQuery(fs, query); err != nil {
		return helptext.FailUsage("network websearch: %v", err)
	}
	if *query != "" {
		return oneShot(context.Background(), *query, *maxResults, nil, os.Stdout)
	}
	return run(context.Background())
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
