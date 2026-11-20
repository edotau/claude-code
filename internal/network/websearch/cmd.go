// Package websearch implements the `network websearch` CLI verb.
package websearch

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/ui/helptext"
	lib "github.com/edotau/claude-code/internal/websearch"
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
		return extractURLs(context.Background(), *extract, lib.Extract)
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

// run wires stdin/stdout to a default (Tavily-backed) server. Entry point for the CLI command.
func run(ctx context.Context) int {
	if _, err := lib.ResolveKey(); err != nil {
		fmt.Fprintf(os.Stderr, "websearch: %v; web_search returns an error until fixed\n", err)
	}
	srv := &lib.Server{}
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		return helptext.Fail("network websearch: %v", err)
	}
	return 0
}

// oneShot runs one Tavily search and writes the text to out (no MCP handshake); non-zero exit on error.
func oneShot(ctx context.Context, query string, maxResults int, search lib.Searcher, out io.Writer) int {
	if strings.TrimSpace(query) == "" {
		return helptext.FailUsage("network websearch: a query is required")
	}
	if maxResults <= 0 {
		maxResults = lib.DefaultMaxResults
	}
	if search == nil {
		search = lib.SearchText
	}
	text, err := search(ctx, query, maxResults)
	if err != nil {
		return helptext.Fail("network websearch: %v", err)
	}
	if _, err := fmt.Fprintln(out, text); err != nil {
		return helptext.Fail("network websearch: could not write result: %v", err)
	}
	return 0
}

// extractFunc reads URLs as markdown blocks and returns the rendered text and count of succeeded pages.
type extractFunc func(ctx context.Context, urls []string) (string, int, error)

// extractURLs prints Extract's `# <url>` blocks to stdout. Exit 1 when every URL failed, else 0.
func extractURLs(ctx context.Context, urls []string, extract extractFunc) int {
	if len(urls) > lib.MaxExtractURLs { // Tavily rejects the whole call over its cap, as the MCP path does
		return helptext.Fail("network websearch --extract: at most %d URLs (got %d)", lib.MaxExtractURLs, len(urls))
	}
	text, ok, err := extract(ctx, urls)
	if err != nil {
		return helptext.Fail("network websearch --extract: %v", err)
	}
	if _, err := fmt.Print(text); err != nil {
		return helptext.Fail("network websearch --extract: could not write result: %v", err)
	}
	if ok == 0 {
		return 1
	}
	return 0
}

// urlList collects repeatable --extract values.
type urlList []string

func (l *urlList) String() string { return strings.Join(*l, ",") }
func (l *urlList) Set(s string) error {
	*l = append(*l, s)
	return nil
}
