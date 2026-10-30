// `memory archive` governs Claude Code's native MEMORY.md/SEARCHABLE.md pair: the cap/settle
// machinery in memory.go owns the six typed bank files BY NAME, so native auto-memory — injected
// every session, uncapped — has no verb at all otherwise.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var memoryArchivePage = helptext.Page{
	Usage: "claude-code memory archive [flags] [<topic>...]",
	About: "Audit the native MEMORY.md store's injected cost (no writes); --apply moves named\n" +
		"topics to or from the archive, or --split N trims MEMORY.md to N pointer lines.",
}

// dupeThreshold is the Jaccard overlap at which two topic files are reported as near-duplicates.
const dupeThreshold = 0.45

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
	dir := memory.NativeDir(memoryRoot(*repo))
	names := fs.Args()

	switch {
	case *dupes:
		return reportNativeDupes(dir)
	case *split >= 0:
		return runSplitNative(dir, *split, *apply)
	case len(names) == 0:
		return reportNativeStats(dir)
	}

	verb, run := "archive", memory.ArchiveNative
	if *restore {
		verb, run = "restore", memory.RestoreNative
	}
	res, err := run(dir, names, *apply)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory archive:", err)
		return 1
	}
	moved := res.Archived
	if *restore {
		moved = res.Restored
	}
	for _, s := range res.Skipped {
		fmt.Fprintln(os.Stderr, "skipped:  ", s)
	}
	if len(moved) == 0 {
		fmt.Println("nothing to " + verb)
		return 0
	}
	if !*apply {
		fmt.Printf("dry-run: would %s %d entr%s: %s\n", verb, len(moved), entrySuffix(len(moved)), strings.Join(moved, ", "))
		fmt.Println("re-run with --apply to move them")
		return 0
	}
	fmt.Printf("%sd %d entr%s -> %s\n", verb, len(moved), entrySuffix(len(moved)), memory.NativeArchiveDir(dir))
	for _, n := range moved {
		fmt.Println("  ", n)
	}
	if !*restore {
		fmt.Println("pointer lines recorded verbatim; `memory archive --restore <name> --apply` puts one back")
	}
	return 0
}

// runSplitNative trims MEMORY.md to keepLines pointer lines; dry run reports the count only.
func runSplitNative(dir string, keepLines int, apply bool) int {
	if !apply {
		st, err := memory.NativeStats(dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "memory archive:", err)
			return 1
		}
		if st.IndexEntries <= keepLines {
			fmt.Printf("dry-run: %s already has %d pointer line%s (cap %d) — nothing to split\n",
				memory.NativeIndexFile, st.IndexEntries, textutil.Plural(st.IndexEntries), keepLines)
			return 0
		}
		fmt.Printf("dry-run: would move %d of %d pointer lines to %s\n",
			st.IndexEntries-keepLines, st.IndexEntries, memory.NativeSearchableFile)
		fmt.Println("re-run with --apply to move them")
		return 0
	}
	moved, err := memory.SplitNativeIndex(dir, keepLines)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory archive:", err)
		return 1
	}
	if moved == 0 {
		fmt.Println("nothing to split")
		return 0
	}
	fmt.Printf("moved %d pointer line%s from %s to %s\n", moved, textutil.Plural(moved), memory.NativeIndexFile, memory.NativeSearchableFile)
	return 0
}

// reportNativeStats prints what the native store costs every session, plus its integrity faults.
func reportNativeStats(dir string) int {
	st, err := memory.NativeStats(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory archive:", err)
		return 1
	}
	fmt.Printf("store:     %s\n", dir)
	fmt.Printf("index:     %s — %d entr%s, %s (injected EVERY session)\n", memory.NativeIndexFile,
		st.IndexEntries, entrySuffix(st.IndexEntries), byteSize(st.IndexBytes))
	fmt.Printf("topics:    %d file%s, %s (read only on recall)\n", st.TopicFiles, textutil.Plural(st.TopicFiles), byteSize(st.TopicBytes))
	if st.IndexEntries > memory.NativeCoreCap {
		fmt.Printf("over cap:  %d pointer line%s over the %d-line injected core — `--split %d --apply` moves the rest to %s\n",
			st.IndexEntries-memory.NativeCoreCap, textutil.Plural(st.IndexEntries-memory.NativeCoreCap), memory.NativeCoreCap,
			memory.NativeCoreCap, memory.NativeSearchableFile)
	}
	if len(st.Dangling) > 0 {
		fmt.Printf("dangling:  %d pointer%s with no topic file: %s\n", len(st.Dangling), textutil.Plural(len(st.Dangling)),
			strings.Join(st.Dangling, ", "))
	}
	if len(st.Orphans) > 0 {
		fmt.Printf("orphans:   %d topic file%s no pointer reaches: %s\n", len(st.Orphans), textutil.Plural(len(st.Orphans)),
			strings.Join(st.Orphans, ", "))
	}
	fmt.Println("archive one: claude-code memory archive <name>... [--apply]   dupes: --dupes   trim: --split N --apply")
	return 0
}

// reportNativeDupes prints near-duplicate clusters and the survivor each collapses into.
func reportNativeDupes(dir string) int {
	clusters, err := memory.NativeDupes(dir, dupeThreshold)
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory archive:", err)
		return 1
	}
	if len(clusters) == 0 {
		fmt.Printf("no topic files overlap at or above %.0f%%\n", dupeThreshold*100)
		return 0
	}
	fmt.Printf("%d near-duplicate cluster%s (Jaccard >= %.0f%%); the survivor is the longest body:\n",
		len(clusters), textutil.Plural(len(clusters)), dupeThreshold*100)
	for _, c := range clusters {
		fmt.Printf("  keep %s  (%.0f%%)\n", c.Keep, c.Score*100)
		for _, l := range c.Losers {
			fmt.Printf("       archive %s\n", l)
		}
	}
	fmt.Println("fold any detail the survivor lacks into it FIRST, then archive the rest")
	return 0
}

// entrySuffix/plural keep the output grammatical without a pluralization dependency.
func entrySuffix(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// byteSize renders a size in the units the rest of the audit output uses.
func byteSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}
