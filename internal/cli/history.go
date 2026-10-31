package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/history"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var historyPage = helptext.Page{
	Usage: "claude-code history <command> [flags]",
	About: "A shadow git repo (state/harness/history) that snapshots the config files, so a\n" +
		"bad render or edit can be diffed and rolled back.",
	Commands: [][2]string{
		{"snapshot", "commit the config files now (--note)"},
		{"log", "recent config commits (-n, --format)"},
		{"diff", "one config commit's patch"},
	},
	Hint: "Config files only. Memory history: git -C ~/.claude/memory log\n\n" +
		"claude-code history <command> -h lists that command's flags.",
}

// cmdHistory reads and writes the shadow config history (state/harness/history, gitignored); memory has its own git.
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
	fmt.Fprintf(os.Stderr, "history: unknown subcommand %q\n", sub)
	historyPage.Fprint(os.Stderr)
	return 2
}

// historySnapshot records the config plane now; unchanged files report so rather than committing.
func historySnapshot(args []string) int {
	fs := helptext.NewFlagSet("history snapshot", helptext.Page{Usage: "claude-code history snapshot [flags]"})
	note := fs.String("note", "manual", "note recorded in the commit subject")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	store := &history.Store{Dir: history.Dir()}
	rc := 0
	cfg, err := store.Snapshot(history.ConfigFiles(), "config", *note)
	if err != nil {
		fmt.Fprintln(os.Stderr, "history: config:", err)
		rc = 1
	}
	printSnapshot("config", cfg)
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

// historyLog prints the config plane's log.
func historyLog(args []string) int {
	fs := helptext.NewFlagSet("history log", helptext.Page{Usage: "claude-code history log [flags]"})
	n := fs.Int("n", 20, "commits per plane")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat("history", err)
	}
	store := &history.Store{Dir: history.Dir()}
	rc := 0
	planes := map[string][]logEntry{}
	for _, plane := range []string{"config"} {
		lines, err := store.Log(*n, plane)
		if err != nil {
			fmt.Fprintf(os.Stderr, "history: %s: %v\n", plane, err)
			rc = 1
		}
		if *format == helptext.JSON {
			planes[plane] = parseLog(lines)
			continue
		}
		printLog(plane, lines)
	}
	if *format == helptext.JSON {
		_ = helptext.WriteJSON(os.Stdout, planes)
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

// historyDiff shows one config-plane commit's patch.
func historyDiff(args []string) int {
	fs := helptext.NewFlagSet("history diff", helptext.Page{Usage: "claude-code history diff [rev]"})
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	rev := ""
	if fs.NArg() > 0 {
		rev = fs.Arg(0)
	}
	plane := "config"
	store := &history.Store{Dir: history.Dir()}
	out, err := store.Diff(rev, plane)
	if err != nil {
		fmt.Fprintln(os.Stderr, "history:", err)
		return 1
	}
	fmt.Println(out)
	return 0
}
