package cli

import (
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var memoryAliasPage = helptext.Page{
	Usage: "claude-code memory alias --repo CHECKOUT --into KEY [flags]",
	About: "Record CHECKOUT as an alias of store KEY and fold any separate store into KEY.",
}

// cmdMemoryAlias is `memory alias`: --dry-run prints the plan and writes nothing.
func cmdMemoryAlias(args []string) int {
	fs := helptext.NewFlagSet("memory alias", memoryAliasPage)
	repo := fs.String("repo", "", "second checkout of the repo to record as an alias")
	into := fs.String("into", "", "store key that absorbs it (see the catalog)")
	dryRun := fs.Bool("dry-run", false, "print the plan; write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *repo == "" || *into == "" || fs.NArg() > 0 {
		return fail("memory alias: --repo and --into are required (usage: %s)", memoryAliasPage.Usage)
	}
	res, err := memory.AliasRepo(memory.AliasOptions{Repo: *repo, Into: *into, DryRun: *dryRun, Now: time.Now()})
	if err != nil {
		return fail("memory alias: %v", err)
	}
	verb := "applied"
	if *dryRun {
		verb = "plan (dry run, nothing written)"
	}
	fmt.Println(verb + ":")
	for _, l := range res.Lines {
		fmt.Println("  " + l)
	}
	return 0
}
