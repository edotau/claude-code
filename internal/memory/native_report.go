package memory

import (
	"fmt"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/textutil"
)

// dupeThreshold is the Jaccard overlap at which two topic files are reported as near-duplicates.
const dupeThreshold = 0.45

// ArchiveReportOptions is one `memory archive`: Dupes, Split>=0 or no Names select a report; else Names move.
type ArchiveReportOptions struct {
	Dir                   string
	Names                 []string
	Apply, Restore, Dupes bool
	Split                 int
}

// ArchiveReport runs the archive verb's flow against the native store at o.Dir; Skipped notes go to errw.
func ArchiveReport(out, errw io.Writer, o ArchiveReportOptions) error {
	switch {
	case o.Dupes:
		return nativeDupesReport(out, o.Dir)
	case o.Split >= 0:
		return splitNativeReport(out, o.Dir, o.Split, o.Apply)
	case len(o.Names) == 0:
		return nativeStatsReport(out, o.Dir)
	}
	verb, run := "archive", ArchiveNative
	if o.Restore {
		verb, run = "restore", RestoreNative
	}
	res, err := run(o.Dir, o.Names, o.Apply)
	if err != nil {
		return err
	}
	moved := res.Archived
	if o.Restore {
		moved = res.Restored
	}
	for _, s := range res.Skipped {
		fmt.Fprintln(errw, "skipped:  ", s)
	}
	reportMoved(out, o, verb, moved)
	return nil
}

// reportMoved prints the dry-run preview or the applied result of an archive/restore.
func reportMoved(out io.Writer, o ArchiveReportOptions, verb string, moved []string) {
	switch {
	case len(moved) == 0:
		fmt.Fprintln(out, "nothing to "+verb)
	case !o.Apply:
		fmt.Fprintf(out, "dry-run: would %s %d entr%s: %s\n", verb, len(moved), entrySuffix(len(moved)), strings.Join(moved, ", "))
		fmt.Fprintln(out, "re-run with --apply to move them")
	default:
		fmt.Fprintf(out, "%sd %d entr%s -> %s\n", verb, len(moved), entrySuffix(len(moved)), NativeArchiveDir(o.Dir))
		for _, n := range moved {
			fmt.Fprintln(out, "  ", n)
		}
		if !o.Restore {
			fmt.Fprintln(out, "pointer lines recorded verbatim; `memory archive --restore <name> --apply` puts one back")
		}
	}
}

// splitNativeReport trims MEMORY.md to keepLines pointer lines; dry run reports the count only.
func splitNativeReport(w io.Writer, dir string, keepLines int, apply bool) error {
	if !apply {
		st, err := NativeStats(dir)
		if err != nil {
			return err
		}
		if st.IndexEntries <= keepLines {
			fmt.Fprintf(w, "dry-run: %s already has %d pointer line%s (cap %d) — nothing to split\n",
				bank.NativeIndexFile, st.IndexEntries, textutil.Plural(st.IndexEntries), keepLines)
			return nil
		}
		fmt.Fprintf(w, "dry-run: would move %d of %d pointer lines to %s\n",
			st.IndexEntries-keepLines, st.IndexEntries, bank.NativeSearchableFile)
		fmt.Fprintln(w, "re-run with --apply to move them")
		return nil
	}
	moved, err := SplitNativeIndex(dir, keepLines)
	if err != nil {
		return err
	}
	if moved == 0 {
		fmt.Fprintln(w, "nothing to split")
		return nil
	}
	fmt.Fprintf(w, "moved %d pointer line%s from %s to %s\n", moved, textutil.Plural(moved), bank.NativeIndexFile, bank.NativeSearchableFile)
	return nil
}

// nativeStatsReport prints what the native store costs every session, plus its integrity faults.
func nativeStatsReport(w io.Writer, dir string) error {
	st, err := NativeStats(dir)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "store:     %s\n", dir)
	fmt.Fprintf(w, "index:     %s — %d entr%s, %s (injected EVERY session)\n", bank.NativeIndexFile,
		st.IndexEntries, entrySuffix(st.IndexEntries), byteSize(st.IndexBytes))
	fmt.Fprintf(w, "topics:    %d file%s, %s (read only on recall)\n", st.TopicFiles, textutil.Plural(st.TopicFiles), byteSize(st.TopicBytes))
	if st.IndexEntries > bank.NativeCoreCap {
		fmt.Fprintf(w, "over cap:  %d pointer line%s over the %d-line injected core — `--split %d --apply` moves the rest to %s\n",
			st.IndexEntries-bank.NativeCoreCap, textutil.Plural(st.IndexEntries-bank.NativeCoreCap), bank.NativeCoreCap,
			bank.NativeCoreCap, bank.NativeSearchableFile)
	}
	if len(st.Dangling) > 0 {
		fmt.Fprintf(w, "dangling:  %d pointer%s with no topic file: %s\n", len(st.Dangling), textutil.Plural(len(st.Dangling)),
			strings.Join(st.Dangling, ", "))
	}
	if len(st.Orphans) > 0 {
		fmt.Fprintf(w, "orphans:   %d topic file%s no pointer reaches: %s\n", len(st.Orphans), textutil.Plural(len(st.Orphans)),
			strings.Join(st.Orphans, ", "))
	}
	fmt.Fprintln(w, "archive one: claude-code memory archive <name>... [--apply]   dupes: --dupes   trim: --split N --apply")
	return nil
}

// nativeDupesReport prints near-duplicate clusters and the survivor each collapses into.
func nativeDupesReport(w io.Writer, dir string) error {
	clusters := NativeDupes(dir, dupeThreshold)
	if len(clusters) == 0 {
		fmt.Fprintf(w, "no topic files overlap at or above %.0f%%\n", dupeThreshold*100)
		return nil
	}
	fmt.Fprintf(w, "%d near-duplicate cluster%s (Jaccard >= %.0f%%); the survivor is the longest body:\n",
		len(clusters), textutil.Plural(len(clusters)), dupeThreshold*100)
	for _, c := range clusters {
		fmt.Fprintf(w, "  keep %s  (%.0f%%)\n", c.Keep, c.Score*100)
		for _, l := range c.Losers {
			fmt.Fprintf(w, "       archive %s\n", l)
		}
	}
	fmt.Fprintln(w, "fold any detail the survivor lacks into it FIRST, then archive the rest")
	return nil
}

// entrySuffix keeps the output grammatical without a pluralization dependency.
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
