package remote

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

type remoteFlags struct {
	host, dir, agent, name, format string
	dryRun, list, status, attach   bool
	interval                       time.Duration
}

func addRemoteFlags(fs *flag.FlagSet) *remoteFlags {
	f := &remoteFlags{}
	fs.StringVar(&f.host, "host", "", "ssh host: alias, MagicDNS/tailnet name, or IP")
	fs.StringVar(&f.dir, "dir", "", "remote working directory (default ~)")
	fs.StringVar(&f.agent, "agent", "claude", "agent CLI: "+strings.Join(Agents, "|"))
	fs.StringVar(&f.name, "name", "", "tmux session name (default cc-<agent>-<id>)")
	fs.BoolVar(&f.dryRun, "dry-run", false, "print the ssh argv instead of running it")
	fs.BoolVar(&f.list, "list", false, "list cc-* tmux sessions on --host")
	fs.BoolVar(&f.status, "status", false, "with --list, classify each pane's state")
	fs.DurationVar(&f.interval, "interval", 0, "with --status, sample every interval (0 = once)")
	fs.BoolVar(&f.attach, "attach", false, "attach to a session on --host (positional: session name)")
	fs.StringVar(&f.format, "format", helptext.Text, "--list output format: text|json")
	return f
}

// Flags are the completion flags of `network remote`, derived from its FlagSet.
func Flags() []helptext.Flag {
	fs := helptext.NewFlagSet("network remote", helptext.Page{})
	addRemoteFlags(fs)
	return helptext.FlagsOf(fs, map[string]func() []string{
		"host":  tailscale.CachedMachineNames,
		"agent": func() []string { return Agents },
	})
}

func Cmd(args []string) int {
	fs := helptext.NewFlagSet("network remote", remotePage)
	f := addRemoteFlags(fs)
	// Interspersed: a trailing --dry-run must not join the task and run it for real.
	if len(args) == 0 || args[0] == "help" {
		fs.Usage()
		return 0
	}
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	mode := f.mode()
	if code := checkRemoteFlags(fs, f, mode); code != 0 {
		return code
	}
	ctx := context.Background()
	resolved, err := tailscale.ResolveHost(ctx, f.host)
	if err != nil {
		return helptext.Fail("network remote: %v", err)
	}
	return dispatchRemoteMode(ctx, resolved, mode, f, rest)
}

// checkRemoteFlags returns a non-zero exit code when --host is missing or the flags disagree with the mode.
func checkRemoteFlags(fs *flag.FlagSet, f *remoteFlags, mode string) int {
	if f.host == "" {
		return helptext.Fail("network remote: --host is required")
	}
	if f.list && f.attach {
		return helptext.FailUsage("network remote: --list and --attach are mutually exclusive")
	}
	if wrong := remoteModeFlags(fs, mode); wrong != nil {
		return helptext.Fail("network remote: %v", wrong)
	}
	return 0
}

func (f remoteFlags) mode() string {
	if f.list {
		return "list"
	}
	if f.attach {
		return "attach"
	}
	return "start"
}

func remoteModeFlags(fs *flag.FlagSet, mode string) error {
	var wrong error
	fs.Visit(func(f *flag.Flag) {
		if owner, ok := dispatchFlagMode[f.Name]; ok && owner != mode && wrong == nil {
			wrong = fmt.Errorf("--%s applies to --%s mode only, not --%s", f.Name, owner, mode)
		}
	})
	return wrong
}

func dispatchRemoteMode(ctx context.Context, host, mode string, f *remoteFlags, rest []string) int {
	switch mode {
	case "list":
		return dispatchList(ctx, host, f.format, f.status, f.interval)
	case "attach":
		if len(rest) != 1 {
			return helptext.Fail("network remote --attach: give exactly one session name")
		}
		return dispatchAttach(host, rest[0])
	default:
		return dispatchStart(ctx, host, f.dir, f.agent, f.name, strings.Join(rest, " "), f.dryRun)
	}
}

func dispatchStart(ctx context.Context, host, dir, agentName, name, task string, dryRun bool) int {
	if task == "" {
		return helptext.Fail("network remote: give a task as the final argument")
	}
	remoteCmd, session, err := BuildCommand(Opts{Dir: dir, Agent: agentName, Name: name, Task: task})
	if err != nil {
		return helptext.Fail("network remote: %v", err)
	}
	local := tailscale.IsLocalHost(ctx, host)
	argv, err := ExecArgs(host, remoteCmd, local, false)
	if err != nil {
		return helptext.Fail("network remote: %v", err)
	}
	if dryRun {
		for i, a := range argv {
			fmt.Printf("argv[%d]: %s\n", i, a)
		}
		fmt.Println("attach:", AttachCommand(host, session, local))
		return 0
	}
	out, err := RunAgent(ctx, argv)
	_, writeErr := os.Stdout.Write(out)
	if err := errors.Join(err, writeErr); err != nil {
		return helptext.Fail("network remote: %v", err)
	}
	fmt.Printf("remote: started %s on %s\n", session, host)
	fmt.Println("attach:", AttachCommand(host, session, local))
	return 0
}

// dispatchList dispatches to the plain session list or, with withStatus, the pane-status sampler.
func dispatchList(ctx context.Context, host, format string, withStatus bool, interval time.Duration) int {
	if err := helptext.CheckFormat(format); err != nil {
		return helptext.BadFormat("network remote", err)
	}
	if !withStatus {
		return dispatchListPlain(ctx, host, format)
	}
	return dispatchListStatus(ctx, host, format, interval)
}

func dispatchListPlain(ctx context.Context, host, format string) int {
	sessions, err := ListSessions(ctx, host, tailscale.IsLocalHost(ctx, host))
	if err != nil {
		return helptext.Fail("network remote --list: %v", err)
	}
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("network remote --list", sessions)
	}
	for _, s := range sessions {
		_, err := fmt.Fprintf(os.Stdout, "%-24s created=%-12s attached=%-5v cmd=%s\n",
			s.Name, s.Created, s.Attached, s.CurrentCmd)
		if err != nil {
			return helptext.Fail("network remote --list: %v", err)
		}
	}
	return 0
}

// dispatchListStatus samples pane status once (interval <= 0) or repeatedly until Ctrl-C.
func dispatchListStatus(ctx context.Context, host, format string, interval time.Duration) int {
	local := tailscale.IsLocalHost(ctx, host)
	err := PollPaneStatus(ctx, host, local, interval, func(s []PaneStatus) error {
		return printPaneStatus(s, format)
	})
	if err != nil {
		return helptext.Fail("network remote --status: %v", err)
	}
	return 0
}

// printPaneStatus writes one sample of pane statuses in the requested format.
func printPaneStatus(statuses []PaneStatus, format string) error {
	if format == helptext.JSON {
		return helptext.WriteJSON(os.Stdout, statuses)
	}
	for _, p := range statuses {
		_, err := fmt.Fprintf(os.Stdout, "%-24s %-6s agent=%-10s state=%-13s %s\n",
			p.Session, p.PaneID, p.Agent, p.State, p.Reason)
		if err != nil {
			return err
		}
	}
	return nil
}

func dispatchAttach(host, session string) int {
	if err := AttachSession(host, session, tailscale.IsLocalHost(context.Background(), host)); err != nil {
		return helptext.Fail("network remote --attach: %v", err)
	}
	return 0
}
