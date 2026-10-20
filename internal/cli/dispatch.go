package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/dispatch"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/proc"
)

// dispatchUsage documents the three modes: start, --list, --attach.
const dispatchUsage = `usage: claude-code dispatch --host <name> [--dir <path>] [--agent <a>]
                           [--name <n>] [--dry-run] "<task>"
       claude-code dispatch --list --host <name> [--status] [--interval <dur>]
                           [--format text|json]
       claude-code dispatch --attach --host <name> <session>
`

// sshTimeout bounds one ssh round trip (start or list); attach replaces the process and has no deadline.
const sshTimeout = 20 * time.Second

func cmdDispatch(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		helptext.Fprint(os.Stderr, dispatchUsage)
		return 2
	}
	fs := flag.NewFlagSet("dispatch", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	fs.SetOutput(os.Stderr)
	host := fs.String("host", "", "ssh host: alias, MagicDNS/tailnet name, or IP")
	dir := fs.String("dir", "", "remote working directory (default ~)")
	agentName := fs.String("agent", "claude", "agent CLI: "+strings.Join(dispatch.Agents, "|"))
	name := fs.String("name", "", "tmux session name (default cc-<agent>-<id>)")
	dryRun := fs.Bool("dry-run", false, "print the ssh argv instead of running it")
	list := fs.Bool("list", false, "list cc-* tmux sessions on --host")
	status := fs.Bool("status", false, "with --list, classify each pane's state")
	interval := fs.Duration("interval", 0, "with --status, sample every interval (0 = once)")
	attach := fs.Bool("attach", false, "attach to a session on --host (positional: session name)")
	format := fs.String("format", outfmt.Text, "--list output format: text|json")
	if fs.Parse(args) != nil {
		return 2
	}
	if *host == "" {
		return fail("dispatch: --host is required")
	}
	ctx := context.Background()
	resolved, err := dispatch.ResolveHost(ctx, *host)
	if err != nil {
		return fail("%v", err)
	}
	switch {
	case *list:
		return dispatchList(ctx, resolved, *format, *status, *interval)
	case *attach:
		rest := fs.Args()
		if len(rest) != 1 {
			return fail("dispatch --attach: give exactly one session name")
		}
		return dispatchAttach(resolved, rest[0])
	default:
		if !dispatch.ValidAgent(*agentName) {
			return fail("dispatch: --agent %q must be one of %s", *agentName, strings.Join(dispatch.Agents, "|"))
		}
		return dispatchStart(ctx, resolved, *dir, *agentName, *name, strings.Join(fs.Args(), " "), *dryRun)
	}
}

func dispatchStart(ctx context.Context, host, dir, agentName, name, task string, dryRun bool) int {
	if task == "" {
		return fail("dispatch: give a task as the final argument")
	}
	remoteCmd, session, err := dispatch.BuildRemoteCommand(dispatch.Opts{Dir: dir, Agent: agentName, Name: name, Task: task})
	if err != nil {
		return fail("%v", err)
	}
	argv := dispatch.ExecArgs(ctx, host, remoteCmd, false)
	if dryRun {
		for i, a := range argv {
			fmt.Printf("argv[%d]: %s\n", i, a)
		}
		fmt.Println("attach:", dispatch.AttachCommand(host, session))
		return 0
	}
	cctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, argv[0], argv[1:]...).CombinedOutput()
	_, writeErr := os.Stdout.Write(out)
	if err := errors.Join(err, writeErr); err != nil {
		return fail("dispatch: %v", err)
	}
	fmt.Printf("dispatch: started %s on %s\n", session, host)
	fmt.Println("attach:", dispatch.AttachCommand(host, session))
	return 0
}

// dispatchList dispatches to the plain session list or, with withStatus, the pane-status sampler.
func dispatchList(ctx context.Context, host, format string, withStatus bool, interval time.Duration) int {
	if err := outfmt.Check(format); err != nil {
		return fail("%v", err)
	}
	if !withStatus {
		return dispatchListPlain(ctx, host, format)
	}
	return dispatchListStatus(ctx, host, format, interval)
}

func dispatchListPlain(ctx context.Context, host, format string) int {
	cctx, cancel := context.WithTimeout(ctx, sshTimeout)
	defer cancel()
	argv := dispatch.ExecArgs(ctx, host, dispatch.ListCommand(), false)
	out, err := exec.CommandContext(cctx, argv[0], argv[1:]...).Output()
	if err != nil {
		return fail("dispatch --list: %v", err)
	}
	sessions := dispatch.ParseSessions(string(out))
	if format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, sessions)
		return 0
	}
	for _, s := range sessions {
		fmt.Printf("%-24s created=%-12s attached=%-5v cmd=%s\n", s.Name, s.Created, s.Attached, s.CurrentCmd)
	}
	return 0
}

// dispatchListStatus samples pane status once (interval <= 0) or repeatedly until Ctrl-C.
func dispatchListStatus(ctx context.Context, host, format string, interval time.Duration) int {
	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	var prev []dispatch.PaneStatus
	for {
		caps, capErr := dispatch.CapturePanes(sigCtx, host, false, dispatch.ExecRunner)
		prev = dispatch.Observe(prev, caps, capErr, time.Now())
		if capErr != nil && len(prev) == 0 {
			return fail("dispatch --status: %v", capErr)
		}
		printPaneStatus(prev, format)
		if interval <= 0 {
			return 0
		}
		select {
		case <-sigCtx.Done():
			return 0
		case <-time.After(interval):
		}
	}
}

// printPaneStatus writes one sample of pane statuses in the requested format.
func printPaneStatus(statuses []dispatch.PaneStatus, format string) {
	if format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, statuses)
		return
	}
	for _, p := range statuses {
		fmt.Printf("%-24s %-6s agent=%-10s state=%-13s %s\n", p.Session, p.PaneID, p.Agent, p.State, p.Reason)
	}
}

func dispatchAttach(host, session string) int {
	if !dispatch.ValidName(session) {
		return fail("dispatch --attach: session %q must match ^[A-Za-z0-9._-]{1,64}$", session)
	}
	argv := dispatch.AttachArgs(host, session)
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return fail("dispatch --attach: %v", err)
	}
	if err := proc.Exec(bin, argv[1:], os.Environ()); err != nil {
		return fail("dispatch --attach: %v", err)
	}
	return 0
}
