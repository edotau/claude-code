package cli

import (
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/recall"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func cmdMemoryRecall(args []string) int {
	fs, repo := memoryRepoFlagSet("recall")
	k := fs.Int("k", 3, "maximum hits")
	all := fs.Bool("all", false, "search the home tier and every repo store")
	load := fs.Int("load", 0, "append the show bodies of the top N hits (4KB cap)")
	explain := fs.Bool("explain", false, "explain hook recall selection and candidate scores")
	format := helptext.FormatFlag(fs)
	terms, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("memory recall", err)
	}
	if len(terms) == 0 {
		return helptext.Fail("memory recall: no query (usage: %s)", memorySubPage("recall").Usage)
	}
	if *explain {
		if *all || *format == helptext.JSON || *load != 0 {
			return helptext.Fail("memory recall: --explain does not support --all, --format json, or --load")
		}
		memory.RecallExplain(os.Stdout, *repo, strings.Join(terms, " "))
		return 0
	}
	hits, _ := recall.SearchRepo(*repo, *all, strings.Join(terms, " "), *k)
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, hits); err != nil {
			return helptext.Fail("memory recall: %v", err)
		}
		return 0
	}
	memory.RecallReport(os.Stdout, os.Stderr, hits, *load)
	return 0
}
