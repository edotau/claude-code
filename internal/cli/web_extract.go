package cli

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/browser"
)

// fieldFlag collects repeatable --field name=selector[@attr] values.
type fieldFlag []browser.Field

func (f *fieldFlag) String() string { return strconv.Itoa(len(*f)) }
func (f *fieldFlag) Set(s string) error {
	fld, err := browser.ParseField(s)
	if err != nil {
		return err
	}
	*f = append(*f, fld)
	return nil
}
func (f *fieldFlag) Placeholder() string { return "NAME=SEL[@ATTR]" }

// extractArgs holds the --extract flag family of `network browser`.
type extractArgs struct {
	url, container, next, pages, loadMore, key, out *string
	scroll, ignoreRobots                            *bool
	maxPages                                        *int
	fields                                          fieldFlag
}

func addExtractFlags(fs *flag.FlagSet) *extractArgs {
	a := &extractArgs{}
	a.url = fs.String("extract", "", "scrape a page's repeated records over CDP (format: jsonl|csv|json)")
	a.container = fs.String("container", "", "CSS selector of one record (with --extract)")
	fs.Var(&a.fields, "field", "column: name=sel text, name=sel@attr attribute (repeatable)")
	a.next = fs.String("next", "", "follow this next link or button until absent (with --extract)")
	a.pages = fs.String("pages", "", "URL template with {page}; stops on an empty page (with --extract)")
	a.scroll = fs.Bool("scroll", false, "scroll until 3 rounds add no records (with --extract)")
	a.loadMore = fs.String("load-more", "", "click this button until it is gone (with --extract)")
	a.maxPages = fs.Int("max-pages", 100, "page, scroll or click cap (with --extract)")
	a.key = fs.String("key", "", "field name to dedupe records on (with --extract)")
	a.out = fs.String("o", "", "write records to this file instead of stdout (with --extract)")
	a.ignoreRobots = fs.Bool("ignore-robots", false, "skip the robots.txt check (with --extract)")
	return a
}

// extractFlagsSet names the first extract-only flag given, "" when none.
func extractFlagsSet(fs *flag.FlagSet) string {
	bad := ""
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "container", "field", "next", "pages", "scroll", "load-more", "max-pages", "key", "o", "ignore-robots":
			if bad == "" {
				bad = f.Name
			}
		}
	})
	return bad
}

// networkBrowserExtract runs --extract; format "" means the jsonl default.
func networkBrowserExtract(a *extractArgs, format string) int {
	if format == "" || format == "text" {
		format = "jsonl"
	}
	opts := browser.ExtractOptions{URL: *a.url, Container: *a.container, Fields: a.fields, Next: *a.next,
		Template: *a.pages, LoadMore: *a.loadMore, Scroll: *a.scroll, MaxPages: *a.maxPages, Key: *a.key,
		Format: format, IgnoreRobots: *a.ignoreRobots}
	if err := opts.Validate(); err != nil {
		return failUsage("network browser: %v", err)
	}
	out := os.Stdout
	if *a.out != "" {
		f, err := os.Create(*a.out)
		if err != nil {
			return fail("network browser: %v", err)
		}
		defer func() { _ = f.Close() }()
		out = f
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := browser.Extract(ctx, browser.NewEnv(), opts, out); err != nil {
		return fail("network browser --extract: %s", strings.TrimSpace(err.Error()))
	}
	return 0
}
