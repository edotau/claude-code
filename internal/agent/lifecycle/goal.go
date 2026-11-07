package lifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/goal"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// goalPage is `agents goal -h`.
var goalPage = helptext.Page{
	Usage: "claude-code agents goal <command> [flags]",
	About: "Record goals and their done check in the ledger, optionally mirrored onto a plan line. " +
		"Nothing runs the check automatically. Every line carries the goal's task id (g-xxxxxx).",
	Commands: [][2]string{
		{"add", "add <what> --check <cmd> [--plan <file>] [--id g-xxxxxx]"},
		{"list", "every goal with its status, timestamp and last check output"},
		{"close", "close <id> [--reason <why>]: abandon a goal"},
	},
	Hint: "--plan links the goal to a `- [ ] … (task: <id>)` line in a plan's ## Tasks.",
}

// goalSubPage is `agents goal <verb> -h`; a leaf page lists no commands.
func goalSubPage(verb, args string) helptext.Page {
	return helptext.Page{Usage: "claude-code agents goal " + verb + " " + args, About: goalPage.About}
}

// Goal is `claude-code agents goal add|list|close`.
func Goal(args []string) int {
	if len(args) == 0 {
		goalPage.Fprint(os.Stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		goalPage.Fprint(os.Stderr)
		return 0
	case "add":
		return goalAdd(args[1:])
	case "list":
		return goalList(args[1:])
	case "close":
		return goalClose(args[1:])
	}
	goalPage.Fprint(os.Stderr)
	return 2
}

func goalAdd(args []string) int {
	fs := helptext.NewFlagSet("agents goal add", goalSubPage("add", "\"<what>\" --check <cmd> [flags]"))
	check := fs.String("check", "", "shell command run in the current directory; the goal is done when it exits 0")
	plan := fs.String("plan", "", "plan file to link by task id (the line is appended to ## Tasks if absent)")
	id := fs.String("id", "", "adopt this task id (an existing plan line's g-xxxxxx)")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 || *check == "" || (*id != "" && !goal.IDPattern.MatchString(*id)) {
		fs.Usage()
		return 2
	}
	cwd, _ := os.Getwd()
	g := goal.Goal{ID: *id, Text: pos[0], Check: *check, Session: os.Getenv(goal.SessionEnv), CWD: cwd}
	if *plan != "" {
		if g.Plan, err = filepath.Abs(*plan); err != nil {
			return helptext.Fail("agents goal add: %v", err)
		}
	}
	g, err = goal.Add(g)
	if err != nil {
		return helptext.Fail("agents goal add: %v", err)
	}
	fmt.Println(g.Line())
	return 0
}

func goalList(args []string) int {
	fs := helptext.NewFlagSet("agents goal list", goalSubPage("list", "[flags]"))
	open := fs.Bool("open", false, "only goals still open")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	for _, g := range goal.All() {
		if *open && g.Status != goal.Open {
			continue
		}
		fmt.Printf("%s — %s %s", g.Line(), g.Status, g.Updated)
		if g.Plan != "" {
			fmt.Printf(" plan=%s", g.Plan)
		}
		if g.LastOut != "" {
			fmt.Printf(" last: %s", strings.ReplaceAll(g.LastOut, "\n", " ⏎ "))
		}
		fmt.Println()
	}
	return 0
}

func goalClose(args []string) int {
	fs := helptext.NewFlagSet("agents goal close", goalSubPage("close", "<id> [flags]"))
	reason := fs.String("reason", "abandoned", "why the goal is closed")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 {
		fs.Usage()
		return 2
	}
	g, err := goal.Load(pos[0])
	if err != nil {
		return helptext.Fail("agents goal close: %v", err)
	}
	if g, err = goal.Tick(g, goal.Closed, *reason, g.LastOut); err != nil {
		return helptext.Fail("agents goal close: %v", err)
	}
	fmt.Printf("%s — closed: %s\n", g.Line(), g.Reason)
	return 0
}
