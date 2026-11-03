// `memory archive` governs Claude Code's native MEMORY.md/SEARCHABLE.md pair: the cap/settle
// machinery in memory.go owns the six typed bank files BY NAME, so native auto-memory — injected
// every session, uncapped — has no verb at all otherwise.
package cli

import (
	"fmt"
	"os"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var memoryArchivePage = helptext.Page{
	Usage: "claude-code memory archive [flags] [<topic>...]",
	About: "Audit the native MEMORY.md store's injected cost (no writes); --apply moves named\n" +
		"topics to or from the archive, or --split N trims MEMORY.md to N pointer lines.",
}

// cmdMemoryArchive audits the native store's injected cost by default (names + stats, no writes);
// --apply moves named topics to/from the archive, or --split N trims MEMORY.md to N pointer lines.
func cmdMemoryArchive(args []string) int {
	fs := helptext.NewFlagSet("memory archive", memoryArchivePage)
	repo := fs.String("repo", "", "project root (default: $CLAUDE_PROJECT_DIR, git toplevel, cwd)")
	apply := fs.Bool("apply", false, "perform the moves/split (default: dry run)")
	restore := fs.Bool("restore", false, "move the named topics back out of the archive")
	dupes := fs.Bool("dupes", false, "report near-duplicate topic files (no writes)")
	split := fs.Int("split", -1, "trim MEMORY.md to N pointers; move rest to SEARCHABLE.md")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	err = memory.ArchiveReport(os.Stdout, os.Stderr, memory.ArchiveReportOptions{Dir: memstore.NativeDir(memstore.Root(*repo)),
		Names: names, Apply: *apply, Restore: *restore, Dupes: *dupes, Split: *split})
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory archive:", err)
		return 1
	}
	return 0
}
