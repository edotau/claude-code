package cli

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/memory"
)

const memoryAliasUsage = `usage: claude-code memory alias --repo CHECKOUT --into KEY [--dry-run]
Records CHECKOUT as an explicit alias of store KEY and folds its separate store (if any) into KEY.`

// cmdMemoryAlias is `memory alias`: --dry-run prints the plan and writes nothing.
func cmdMemoryAlias(args []string) int {
	fs := flag.NewFlagSet("memory alias", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	repo := fs.String("repo", "", "second checkout of the repo to record as an alias")
	into := fs.String("into", "", "store key that absorbs it (see the catalog)")
	dryRun := fs.Bool("dry-run", false, "print the plan; write nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *repo == "" || *into == "" || fs.NArg() > 0 {
		return fail("memory alias: --repo and --into are required (%s)", memoryAliasUsage)
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
