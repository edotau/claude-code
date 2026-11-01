// `memory archive` governs Claude Code's native MEMORY.md/SEARCHABLE.md pair: the cap/settle
// machinery in memory.go owns the six typed bank files BY NAME, so native auto-memory — injected
// every session, uncapped — has no verb at all otherwise.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var memoryArchivePage = helptext.Page{
	Usage: "claude-code memory archive [flags] [<topic>...]",
	About: "Audit the native MEMORY.md store's injected cost (no writes); --apply moves named\n" +
		"topics to or from the archive, or --split N trims MEMORY.md to N pointer lines.",
}

// archiveBoolFlags/archiveValueFlags are this verb's flags, for hoistArchiveFlags to recognize.
var (
	archiveBoolFlags  = map[string]bool{"apply": true, "restore": true, "dupes": true}
	archiveValueFlags = map[string]bool{"repo": true, "split": true}
)

// hoistArchiveFlags moves this verb's flags (and the value token a value-flag consumes) ahead of the
// positionals so flag order does not matter — "topic --apply" is as valid as "--apply topic". flag.Parse
// stops at the first positional, so a trailing --apply would otherwise be swallowed as a topic name and
// silently report a successful dry run. An unrecognized flag-shaped token is an error, not a name.
func hoistArchiveFlags(args []string) ([]string, error) {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			rest = append(rest, a)
			continue
		}
		name, _, hasEq := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch {
		case archiveBoolFlags[name], name == "h", name == "help":
			flags = append(flags, a)
		case archiveValueFlags[name]:
			flags = append(flags, a)
			if !hasEq && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		default:
			return nil, fmt.Errorf("unknown flag %q (known: --repo --split --apply --restore --dupes)", a)
		}
	}
	return append(flags, rest...), nil
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
	args, err := hoistArchiveFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory archive:", err)
		return 2
	}
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	err = memory.ArchiveReport(os.Stdout, os.Stderr, memory.ArchiveReportOptions{Dir: memstore.NativeDir(memstore.Root(*repo)),
		Names: fs.Args(), Apply: *apply, Restore: *restore, Dupes: *dupes, Split: *split})
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory archive:", err)
		return 1
	}
	return 0
}
