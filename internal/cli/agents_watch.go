package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Seams for `agents watch`: the recheck interval and the session owner; tests replace them.
var (
	watchPoll         = 30 * time.Second
	watchSessionOwner = inflight.SessionOwner
)

// watchWidth is the CLI's column cap for each output line.
const watchWidth = 80

var watchPage = helptext.Page{Usage: "claude-code agents watch [flags]",
	About: "Check this session's dispatch jobs and print each stale one: dead with its record running, alive " +
		"past HARNESS_WATCH_AGE (10m; Tier: fast 0.5x, deep 2x) with its log silent past HARNESS_WATCH_SILENCE " +
		"(5m), still running after its relay finished, or finished 5m with its result uncollected; each with " +
		"its last log line and the next command. Without --once it rechecks every 30s until one is stale or " +
		"none is left to watch. Reads only; the hooks' automatic check is HARNESS_WATCH (off|false|0).",
	Hint: "Exit 0 nothing stale, 1 failed, 2 usage, 3 a job is stale."}

// cmdAgentsWatch is `agents watch [--once] [--format json]`.
func cmdAgentsWatch(args []string) int {
	fs := helptext.NewFlagSet("agents watch", watchPage)
	once := fs.Bool("once", false, "check once and exit")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("agents watch", err)
	}
	if len(pos) > 0 {
		fs.Usage()
		return 2
	}
	owner := watchSessionOwner()
	if owner <= 1 {
		return helptext.Fail("agents watch: no Claude Code session behind this shell")
	}
	return watchLoop(owner, *once, *format)
}

// watchLoop checks every watchPoll until a job is stale, nothing is left to watch or --once; ^C prints the last pass.
func watchLoop(owner int, once bool, format string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	for {
		rep, err := agent.Watch(owner, time.Now(), agent.WatchLimitsFromEnv())
		if err != nil {
			return helptext.Fail("agents watch: %v", err)
		}
		if once || len(rep.Stale) > 0 || rep.Pending == 0 {
			return printWatch(rep, format)
		}
		select {
		case <-ctx.Done():
			return printWatch(rep, format)
		case <-time.After(watchPoll):
		}
	}
}

// printWatch prints the report and returns the exit code: 3 when a job is stale.
func printWatch(rep agent.WatchReport, format string) int {
	if format == helptext.JSON {
		if code := helptext.WriteJSONOrFail("agents watch", rep); code != 0 {
			return code
		}
	} else {
		if len(rep.Stale) == 0 {
			fmt.Printf("no stale jobs (%d still watched)\n", rep.Pending)
		}
		for _, s := range rep.Stale {
			for _, l := range agent.WatchLines(s, watchWidth) {
				fmt.Println(l)
			}
		}
	}
	if len(rep.Stale) > 0 {
		return 3
	}
	return 0
}
