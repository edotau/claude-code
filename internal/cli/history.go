package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var historyPage = helptext.Page{
	Usage: "claude-code history <command> [flags]",
	About: "A shadow git repo (state/harness/history) that snapshots the config files, so a\n" +
		"bad render or edit can be found with log and diff.",
	Commands: [][2]string{
		{"snapshot", "commit the config files now (--note)"},
		{"log", "recent config commits (-n, --format)"},
		{"diff", "one config commit's patch"},
	},
	Hint: "Config files only. Memory history: git -C ~/.claude/memory log\n\n" +
		"claude-code history <command> -h lists that command's flags.",
}

// cmdHistory is the `history` verb over the shadow config history (state/harness/history, gitignored).
func cmdHistory(args []string) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "", "-h", "--help", "help":
		historyPage.Fprint(os.Stdout)
		return 0
	case "snapshot":
		return historySnapshot(args)
	case "log":
		return historyLog(args)
	case "diff":
		return historyDiff(args)
	}
	return unknownSub("history", sub, historyPage)
}

// historySnapshot records the config plane now; unchanged files report so rather than committing.
func historySnapshot(args []string) int {
	fs := helptext.NewFlagSet("history snapshot", helptext.Page{Usage: "claude-code history snapshot [flags]",
		About: "Commit the contested config files now; report unchanged, absent or held back."})
	note := fs.String("note", "manual", "note recorded in the commit subject")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	cfg, err := configure.SnapshotConfig(*note)
	if err != nil {
		fmt.Fprintln(os.Stderr, "history: config:", err)
		return 1
	}
	printSnapshot(cfg)
	return 0
}

// printSnapshot reports the config plane's outcome; the HELD BACK line (a credential-shaped file) is the one to act on.
func printSnapshot(r configure.SnapshotResult) {
	switch {
	case r.Commit != "":
		fmt.Printf("%-7s %s  %s\n", "config:", r.Commit, strings.Join(r.Changed, ", "))
	default:
		fmt.Printf("%-7s unchanged\n", "config:")
	}
	if len(r.Absent) > 0 { // one line: absent files are a fact about the box, not a problem per file
		fmt.Printf("%-7s absent: %s\n", "", strings.Join(r.Absent, ", "))
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(os.Stderr, "%-7s HELD BACK %s: %s\n", "config:", s.Label, s.Reason)
	}
}

// historyLog prints the config plane's log.
func historyLog(args []string) int {
	fs := helptext.NewFlagSet("history log", helptext.Page{Usage: "claude-code history log [flags]", About: "List recent config commits, newest first."})
	n := fs.Int("n", 20, "commits to show")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("history", err)
	}
	if *n < 0 {
		return helptext.FailUsage("history log: -n must be >= 0, got %d", *n)
	}
	store := &configure.HistoryStore{Dir: configure.HistoryDir()}
	lines, err := store.Log(*n)
	if err != nil { // a failed read is not an empty history: say so, print nothing as if it were
		fmt.Fprintf(os.Stderr, "history: config: %v\n", err)
		return 1
	}
	if *format == helptext.JSON {
		return helptext.WriteJSONOrFail("history log", map[string][]configure.HistoryEntry{"config": configure.ParseHistoryLog(lines)})
	}
	printLog(lines)
	return 0
}

// printLog prints the config plane's header and lines; an empty history says so rather than printing nothing.
func printLog(lines []string) {
	fmt.Printf("config: %s\n", configure.HistoryDir())
	if len(lines) == 0 {
		fmt.Println("  (no history yet)")
		return
	}
	for _, l := range lines {
		fmt.Println("  " + l)
	}
}

// historyDiff shows one config-plane commit's patch.
func historyDiff(args []string) int {
	fs := helptext.NewFlagSet("history diff", helptext.Page{Usage: "claude-code history diff [rev]",
		About: "Show a config commit's patch; <rev> defaults to the latest config commit."})
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	store := &configure.HistoryStore{Dir: configure.HistoryDir()}
	out, err := store.Diff(firstPositional(fs.Args()))
	if err != nil {
		fmt.Fprintln(os.Stderr, "history:", err)
		return 1
	}
	fmt.Println(out)
	return 0
}

// historySpec returns the history completion tree.
func historySpec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"snapshot": {Flags: []helptext.Flag{{Name: "note"}}},
		"log": {Flags: []helptext.Flag{{Name: "n"},
			helptext.CompletionFormatFlag()}},
		"diff": {},
	}}
}
