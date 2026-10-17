package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/history"
	"github.com/edotau/claude-code/internal/outfmt"
)

const historyUsage = `usage: claude-code history snapshot [--note N]
       claude-code history log [-n N] [--memory|--config]
       claude-code history diff [rev] [--memory]`

// cmdHistory reads and writes the shadow config/memory history: one repo under state/harness/history
// (gitignored), covering the contested config files and the memory view as two labeled "planes" in
// the same store (internal/history).
func cmdHistory(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "", "-h", "--help":
		fmt.Println(historyUsage)
		return 0
	case "snapshot":
		return historySnapshot(args)
	case "log":
		return historyLog(args)
	case "diff":
		return historyDiff(args)
	}
	fmt.Fprintf(os.Stderr, "history: unknown subcommand %q\n%s\n", sub, historyUsage)
	return 2
}

// historySnapshot records both planes now. A plane that did not change reports so rather than
// committing — "there is a commit" must keep meaning "something moved".
func historySnapshot(args []string) int {
	fs := flag.NewFlagSet("history snapshot", flag.ContinueOnError)
	note := fs.String("note", "manual", "note recorded in the commit subject")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	store := &history.Store{Dir: history.Dir()}
	rc := 0
	cfg, err := store.Snapshot(history.ConfigFiles(), "config", *note)
	if err != nil {
		fmt.Fprintln(os.Stderr, "history: config:", err)
		rc = 1
	}
	printSnapshot("config", cfg)
	mem, err := store.Snapshot(history.MemoryFiles(), "memory", *note)
	if err != nil {
		fmt.Fprintln(os.Stderr, "history: memory:", err)
		rc = 1
	}
	printSnapshot("memory", mem)
	return rc
}

// printSnapshot reports one plane's outcome: what was committed, what is absent, and what was HELD
// BACK (a credential-shaped file) — the last one is the line a reader must act on.
func printSnapshot(plane string, r history.Result) {
	switch {
	case r.Commit != "":
		fmt.Printf("%-7s %s  %s\n", plane+":", r.Commit, strings.Join(r.Changed, ", "))
	default:
		fmt.Printf("%-7s unchanged\n", plane+":")
	}
	if len(r.Absent) > 0 { // one line: absent files are a fact about the box, not a problem per file
		fmt.Printf("%-7s absent: %s\n", "", strings.Join(r.Absent, ", "))
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(os.Stderr, "%-7s HELD BACK %s: %s\n", plane+":", s.Label, s.Reason)
	}
}

// historyLog prints both planes' logs unless one is selected. Labeled per plane: the two are
// independent histories, and an unlabeled merge would read as one timeline that never existed.
func historyLog(args []string) int {
	fs := flag.NewFlagSet("history log", flag.ContinueOnError)
	n := fs.Int("n", 20, "commits per plane")
	memOnly := fs.Bool("memory", false, "the memory bank only")
	cfgOnly := fs.Bool("config", false, "the config files only")
	format := outfmt.Flag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := outfmt.Check(*format); err != nil {
		fmt.Fprintln(os.Stderr, "history:", err)
		return 2
	}
	store := &history.Store{Dir: history.Dir()}
	rc := 0
	planes := map[string][]logEntry{}
	for _, plane := range []string{"config", "memory"} {
		if (plane == "config" && *memOnly) || (plane == "memory" && *cfgOnly) {
			continue
		}
		lines, err := store.Log(*n, plane)
		if err != nil {
			fmt.Fprintf(os.Stderr, "history: %s: %v\n", plane, err)
			rc = 1
		}
		if *format == outfmt.JSON {
			planes[plane] = parseLog(lines)
			continue
		}
		printLog(plane, lines)
	}
	if *format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, planes)
	}
	return rc
}

// logEntry is one `history log` line: Store.Log formats "%h %Y-%m-%d %H:%M <plane>: <subject>".
type logEntry struct {
	Rev     string `json:"rev"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

func parseLog(lines []string) []logEntry {
	out := []logEntry{}
	for _, l := range lines {
		f := strings.SplitN(l, " ", 4)
		if len(f) < 4 {
			continue
		}
		out = append(out, logEntry{Rev: f[0], Date: f[1] + " " + f[2], Subject: f[3]})
	}
	return out
}

// printLog prints one plane's header and lines; an empty history says so rather than printing nothing.
func printLog(plane string, lines []string) {
	fmt.Printf("%s: %s\n", plane, history.Dir())
	if len(lines) == 0 {
		fmt.Println("  (no history yet)")
		return
	}
	for _, l := range lines {
		fmt.Println("  " + l)
	}
}

// historyDiff shows one commit's patch. The config plane is the default target: it is the one another
// tool rewrites behind your back, so it is the one a diff is usually chasing.
func historyDiff(args []string) int {
	fs := flag.NewFlagSet("history diff", flag.ContinueOnError)
	mem := fs.Bool("memory", false, "diff the memory bank history instead of the config files")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rev := ""
	if fs.NArg() > 0 {
		rev = fs.Arg(0)
	}
	plane := "config"
	if *mem {
		plane = "memory"
	}
	store := &history.Store{Dir: history.Dir()}
	out, err := store.Diff(rev, plane)
	if err != nil {
		fmt.Fprintln(os.Stderr, "history:", err)
		return 1
	}
	fmt.Println(out)
	return 0
}
