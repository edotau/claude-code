package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/network/tailscale"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// remotePage documents the three modes (start, --list, --attach), each with its own usage line.
var remotePage = helptext.Page{
	Usage: "claude-code network remote --host <name> [flags] \"<task>\"\n" +
		"claude-code network remote --host <name> --list [flags]\n" +
		"claude-code network remote --host <name> --attach <session>",
	About: "Start an agent CLI in a detached tmux session on a tailnet host over ssh, then\n" +
		"list its cc-* sessions (--list) or attach to one (--attach <session>).",
}

// dispatchFlagMode maps a mode-specific flag to the mode that owns it; --host/--list/--attach apply to every mode.
var dispatchFlagMode = map[string]string{
	"dir": "start", "agent": "start", "name": "start", "dry-run": "start",
	"format": "list", "status": "list", "interval": "list",
}

func cmdNetworkRemote(args []string) int {
	remoteAgents := strings.Join(tailscale.RemoteAgents, "|")
	fs := helptext.NewFlagSet("network remote", remotePage)
	host := fs.String("host", "", "ssh host: alias, MagicDNS/tailnet name, or IP")
	dir := fs.String("dir", "", "remote working directory (default ~)")
	agentName := fs.String("agent", "claude", "agent CLI: "+remoteAgents)
	name := fs.String("name", "", "tmux session name (default cc-<agent>-<id>)")
	dryRun := fs.Bool("dry-run", false, "print the ssh argv instead of running it")
	list := fs.Bool("list", false, "list cc-* tmux sessions on --host")
	status := fs.Bool("status", false, "with --list, classify each pane's state")
	interval := fs.Duration("interval", 0, "with --status, sample every interval (0 = once)")
	attach := fs.Bool("attach", false, "attach to a session on --host (positional: session name)")
	format := fs.String("format", helptext.Text, "--list output format: text|json")
	// Interspersed: a trailing --dry-run must not join the task and run it for real.
	if len(args) == 0 || args[0] == "help" {
		fs.Usage()
		return 0
	}
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *host == "" {
		return fail("network remote: --host is required")
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
		return fail("network remote: %v", wrong)
	}
	ctx := context.Background()
	resolved, err := tailscale.ResolveHost(ctx, *host)
	if err != nil {
		return fail("%v", err)
	}
	switch mode {
	case "list":
		return dispatchList(ctx, resolved, *format, *status, *interval)
	case "attach":
		if len(rest) != 1 {
			return fail("network remote --attach: give exactly one session name")
		}
		return dispatchAttach(resolved, rest[0])
	default:
		if !tailscale.ValidAgent(*agentName) {
			return fail("network remote: --agent %q must be one of %s", *agentName, remoteAgents)
		}
		return dispatchStart(ctx, resolved, *dir, *agentName, *name, strings.Join(rest, " "), *dryRun)
	}
}

func dispatchStart(ctx context.Context, host, dir, agentName, name, task string, dryRun bool) int {
	if task == "" {
		return fail("network remote: give a task as the final argument")
	}
	remoteCmd, session, err := tailscale.BuildRemoteCommand(tailscale.RemoteOpts{Dir: dir, Agent: agentName, Name: name, Task: task})
	if err != nil {
		return fail("%v", err)
	}
	argv := tailscale.ExecArgs(ctx, host, remoteCmd, false)
	if dryRun {
		for i, a := range argv {
			fmt.Printf("argv[%d]: %s\n", i, a)
		}
		fmt.Println("attach:", tailscale.AttachCommand(host, session))
		return 0
	}
	out, err := tailscale.RunRemote(ctx, argv)
	_, writeErr := os.Stdout.Write(out)
	if err := errors.Join(err, writeErr); err != nil {
		return fail("network remote: %v", err)
	}
	fmt.Printf("remote: started %s on %s\n", session, host)
	fmt.Println("attach:", tailscale.AttachCommand(host, session))
	return 0
}

// dispatchList dispatches to the plain session list or, with withStatus, the pane-status sampler.
func dispatchList(ctx context.Context, host, format string, withStatus bool, interval time.Duration) int {
	if err := helptext.CheckFormat(format); err != nil {
		return badFormat("", err)
	}
	if !withStatus {
		return dispatchListPlain(ctx, host, format)
	}
	return dispatchListStatus(ctx, host, format, interval)
}

func dispatchListPlain(ctx context.Context, host, format string) int {
	sessions, err := tailscale.ListRemoteSessions(ctx, host)
	if err != nil {
		return fail("network remote --list: %v", err)
	}
	if format == helptext.JSON {
		return writeJSONOrFail("network remote --list", sessions)
	}
	for _, s := range sessions {
		fmt.Printf("%-24s created=%-12s attached=%-5v cmd=%s\n", s.Name, s.Created, s.Attached, s.CurrentCmd)
	}
	return 0
}

// dispatchListStatus samples pane status once (interval <= 0) or repeatedly until Ctrl-C.
func dispatchListStatus(ctx context.Context, host, format string, interval time.Duration) int {
	err := tailscale.PollPaneStatus(ctx, host, interval, func(s []tailscale.PaneStatus) error {
		return printPaneStatus(s, format)
	})
	if err != nil {
		return fail("network remote --status: %v", err)
	}
	return 0
}

// printPaneStatus writes one sample of pane statuses in the requested format.
func printPaneStatus(statuses []tailscale.PaneStatus, format string) error {
	if format == helptext.JSON {
		return helptext.WriteJSON(os.Stdout, statuses)
	}
	for _, p := range statuses {
		fmt.Printf("%-24s %-6s agent=%-10s state=%-13s %s\n", p.Session, p.PaneID, p.Agent, p.State, p.Reason)
	}
	return nil
}

func dispatchAttach(host, session string) int {
	if err := tailscale.AttachSession(host, session); err != nil {
		return fail("network remote --attach: %v", err)
	}
	return 0
}
