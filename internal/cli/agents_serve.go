package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent/serve"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var servePage = helptext.Page{Usage: "claude-code agents serve [--start|--stop|--restart|--status|--serve]",
	About: "A local daemon that admits detached `agents ask --background` jobs per (provider, profile) pool: " +
		"a job waits while its pool is at its cap or cooling after a 429, then runs as it would without " +
		"the daemon. No daemon, or a refusal, runs the job at once. --restart drains: waiting jobs re-queue " +
		"on the new daemon. Not served on Windows. No flag = --status (the queue view).",
	Hint: "Exit 0, 1 failed or not running (--status), 2 usage."}

type serveOpts struct {
	start, stop, restart, status, fg bool
	ceiling                          int
	format                           *string
}

// cmdAgentsServe is `agents serve`: at most one lifecycle flag, no flag = --status.
func cmdAgentsServe(args []string) int {
	fs := helptext.NewFlagSet("agents serve", servePage)
	o := serveOpts{}
	fs.BoolVar(&o.start, "start", false, "start the daemon detached (restarts one on an older build)")
	fs.BoolVar(&o.stop, "stop", false, "stop the daemon; jobs then run directly")
	fs.BoolVar(&o.restart, "restart", false, "drain the daemon and start this binary's")
	fs.BoolVar(&o.status, "status", false, "daemon pid and the queue per pool; exit 1 when not running")
	fs.BoolVar(&o.fg, "serve", false, "run the daemon in the foreground until interrupted")
	fs.IntVar(&o.ceiling, "cap", 0, "with --start/--restart/--serve: cap every pool at N (0 = account limit)")
	o.format = helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if code, ok := o.check(fs); !ok {
		return code
	}
	switch {
	case o.fg:
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return serveResult(serve.Serve(ctx, o.ceiling))
	case o.start:
		st, err := serve.Start(o.ceiling)
		return serveStarted("started", st, err)
	case o.restart:
		return serveRestart(fs, o.ceiling)
	case o.stop:
		st, err := serve.Stop()
		return serveStopped(st, err)
	}
	return serveStatus(*o.format)
}

// modes counts the lifecycle flags given.
func (o serveOpts) modes() int {
	n := 0
	for _, b := range []bool{o.start, o.stop, o.restart, o.status, o.fg} {
		if b {
			n++
		}
	}
	return n
}

func (o serveOpts) check(fs *flag.FlagSet) (int, bool) {
	switch {
	case fs.NArg() > 0:
		return helptext.FailUsage("agents serve: unexpected argument %q", fs.Arg(0)), false
	case o.modes() > 1:
		return helptext.FailUsage("agents serve: at most one of --start, --stop, --restart, --status, --serve"), false
	case o.ceiling < 0:
		return helptext.FailUsage("agents serve: --cap must be 0 or more"), false
	case flagSet(fs, "cap") && !o.start && !o.restart && !o.fg:
		return helptext.FailUsage("agents serve: --cap needs --start, --restart or --serve"), false
	}
	if err := helptext.CheckFormat(*o.format); err != nil {
		return helptext.BadFormat("agents serve", err), false
	}
	return 0, true
}

// serveRestart restarts the daemon; an untyped --cap keeps the running daemon's.
func serveRestart(fs *flag.FlagSet, ceiling int) int {
	if !flagSet(fs, "cap") {
		ceiling = -1
	}
	old, cur, err := serve.Restart(ceiling)
	if err == nil && old.PID != 0 {
		fmt.Printf("agents serve: drained pid %d\n", old.PID)
	}
	return serveStarted("restarted", cur, err)
}

func serveResult(err error) int {
	if err != nil {
		return helptext.Fail("%v", err)
	}
	return 0
}

func serveStarted(verb string, st serve.State, err error) int {
	if err != nil {
		return helptext.Fail("%v", err)
	}
	fmt.Printf("agents serve: %s pid %d on %s\n", verb, st.PID, serve.SockPath())
	return 0
}

func serveStopped(st serve.State, err error) int {
	if errors.Is(err, serve.ErrNotRunning) {
		fmt.Println("agents serve: not running")
		return 0
	}
	if err != nil {
		return helptext.Fail("%v", err)
	}
	fmt.Printf("agents serve: stopped pid %d\n", st.PID)
	return 0
}

// serveStatus prints the daemon and its queue view; exit 1 when no daemon answers.
func serveStatus(format string) int {
	st, err := serve.QueryStatus(2 * time.Second)
	if err != nil {
		if format == helptext.JSON {
			_ = helptext.WriteJSON(os.Stdout, struct {
				Running bool `json:"running"`
			}{})
		} else {
			fmt.Println("agents serve: not running; background jobs run directly")
		}
		return 1
	}
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("agents serve", st)
	}
	fmt.Printf("pid %d since %s cap %d\n", st.PID, st.Started.Local().Format(time.TimeOnly), st.Cap)
	for _, p := range st.Pools {
		fmt.Println(poolLine(p))
	}
	return 0
}

// poolLine is one pool row: load against cap, cool-down end and the waiting ids.
func poolLine(p serve.PoolRow) string {
	line := fmt.Sprintf("%-24s %d/%d", p.Pool, p.Running, p.Cap)
	if !p.Cooling.IsZero() {
		line += " cooling until " + p.Cooling.Local().Format(time.TimeOnly)
	}
	if len(p.Waiting) > 0 {
		ids := make([]string, 0, len(p.Waiting))
		for _, w := range p.Waiting {
			ids = append(ids, w.ID)
		}
		line += fmt.Sprintf(" waiting %d: %s", len(ids), strings.Join(ids, " "))
	}
	return line
}
