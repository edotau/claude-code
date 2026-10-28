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

// dispatchPage documents the three modes: start, --list, --attach; each has its own usage line
// since their flags and positionals are not interchangeable (--attach takes exactly one session).
var dispatchPage = helptext.Page{
	Usage: "claude-code remote --host <name> [flags] \"<task>\"\n" +
		"claude-code remote --host <name> --list [flags]\n" +
		"claude-code remote --host <name> --attach <session>",
	About: "Start an agent CLI in a detached tmux session on a tailnet host over ssh, then\n" +
		"list its cc-* sessions (--list) or attach to one (--attach <session>).",
}

// dispatchFlagMode maps a mode-specific flag to the mode that owns it; --host/--list/--attach apply to every mode.
var dispatchFlagMode = map[string]string{
	"dir": "start", "agent": "start", "name": "start", "dry-run": "start",
	"format": "list", "status": "list", "interval": "list",
}

// sshTimeout bounds one ssh round trip (start or list); attach replaces the process and has no deadline.
const sshTimeout = 20 * time.Second

func cmdDispatch(args []string) int {
	fs := helptext.NewFlagSet("remote", dispatchPage)
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
	// Interspersed: a trailing --dry-run must not join the task and run it for real.
	if len(args) == 0 || args[0] == "help" {
		fs.Usage()
		return 0
	}
	rest, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *host == "" {
		return fail("remote: --host is required")
	}
	mode := "start"
	switch {
	case *list:
		mode = "list"
	case *attach:
		mode = "attach"
	}
	var wrong error
	fs.Visit(func(f *flag.Flag) {
		if owner, ok := dispatchFlagMode[f.Name]; ok && owner != mode && wrong == nil {
			wrong = fmt.Errorf("--%s applies to --%s mode only, not --%s", f.Name, owner, mode)
		}
	})
	if wrong != nil {
		return fail("remote: %v", wrong)
	}
	ctx := context.Background()
	resolved, err := dispatch.ResolveHost(ctx, *host)
	if err != nil {
		return fail("%v", err)
	}
	switch mode {
	case "list":
		return dispatchList(ctx, resolved, *format, *status, *interval)
	case "attach":
		if len(rest) != 1 {
			return fail("remote --attach: give exactly one session name")
		}
		return dispatchAttach(resolved, rest[0])
	default:
		if !dispatch.ValidAgent(*agentName) {
			return fail("remote: --agent %q must be one of %s", *agentName, strings.Join(dispatch.Agents, "|"))
		}
		return dispatchStart(ctx, resolved, *dir, *agentName, *name, strings.Join(rest, " "), *dryRun)
	}
}

func dispatchStart(ctx context.Context, host, dir, agentName, name, task string, dryRun bool) int {
	if task == "" {
		return fail("remote: give a task as the final argument")
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
	n, writeErr := os.Stdout.Write(out)
	if writeErr == nil && n < len(out) {
		writeErr = fmt.Errorf("short write to stdout: wrote %d of %d bytes", n, len(out))
	}
	if err := errors.Join(err, writeErr); err != nil {
		return fail("remote: %v", err)
	}
	fmt.Printf("remote: started %s on %s\n", session, host)
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
		return fail("remote --list: %v", err)
	}
	sessions := dispatch.ParseSessions(string(out))
	if format == outfmt.JSON {
		if err := outfmt.Write(os.Stdout, sessions); err != nil {
			return fail("remote --list: %v", err)
		}
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
			return fail("remote --status: %v", capErr)
		}
		if err := printPaneStatus(prev, format); err != nil {
			return fail("remote --status: %v", err)
		}
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
func printPaneStatus(statuses []dispatch.PaneStatus, format string) error {
	if format == outfmt.JSON {
		return outfmt.Write(os.Stdout, statuses)
	}
	for _, p := range statuses {
		fmt.Printf("%-24s %-6s agent=%-10s state=%-13s %s\n", p.Session, p.PaneID, p.Agent, p.State, p.Reason)
	}
	return nil
}

func dispatchAttach(host, session string) int {
	if !dispatch.ValidName(session) {
		return fail("remote --attach: session %q must match ^[A-Za-z0-9._-]{1,64}$", session)
	}
	argv := dispatch.AttachArgs(host, session)
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return fail("remote --attach: %v", err)
	}
	if err := proc.Exec(bin, argv[1:], os.Environ()); err != nil {
		return fail("remote --attach: %v", err)
	}
	return 0
}
