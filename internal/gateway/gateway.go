// Package gateway owns the gateway command tree.
package gateway

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxy/server"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var gatewayPage = helptext.Page{
	Usage: "claude-code gateway [command] [flags]",
	About: "Where a request goes: the agent + model pick (router) and the loopback proxy\n" +
		"(proxy). Bare: the router pick, then the proxy status.",
	Commands: [][2]string{
		{"router", "which agent + model a task routes to, and why"},
		{"proxy", "the loopback proxy: --start|--stop|--restart|--status|--serve"},
		{"token", "print only the proxy credential for a credential helper"},
	},
	Hint: "claude-code gateway <command> -h lists that command's flags.",
}

// Run dispatches the gateway verb tree.
func Run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			gatewayPage.Fprint(os.Stderr)
			return 0
		case "router":
			return cmdGatewayRouter(args[1:])
		case "proxy":
			return cmdGatewayProxy(args[1:])
		case "token":
			return cmdGatewayToken(args[1:])
		}
		if !strings.HasPrefix(args[0], "-") {
			return helptext.Fail("gateway: unknown command %q (router|proxy|token)", args[0])
		}
	}
	return gatewayRoute(args, true)
}

// printProxyState renders the proxy status block and writes it once; a write error is returned.
func printProxyState(ps proxy.Status) error {
	var buf bytes.Buffer
	if ps.Running {
		fmt.Fprintf(&buf, "proxy     running  pid %d  %s  since %s\n", ps.PID, ps.URL, ps.Started)
	} else {
		fmt.Fprintf(&buf, "proxy     not running  (log %s)\n", ps.Log)
	}
	if len(ps.Fallback) == 0 {
		buf.WriteString("fallback  none\n")
	} else {
		fmt.Fprintln(&buf, "fallback ", strings.Join(ps.Fallback, " → "))
	}
	_, err := os.Stdout.Write(buf.Bytes())
	return err
}

// proxyOpts are `gateway proxy`'s flag values.
type proxyOpts struct {
	start, stop, restart, status, serve bool
	port                                int
	format                              *string
}

func proxyFlags() (*flag.FlagSet, *proxyOpts) {
	fs := helptext.NewFlagSet("gateway proxy", helptext.Page{
		Usage: "claude-code gateway proxy [--start|--stop|--restart|--status|--serve]", // FLAGS lists the rest
		About: "The loopback proxy that translates Anthropic requests for OpenAI-style\n" +
			"providers and falls back across the registry's fallback chain. No flag = --status.",
	})
	o := &proxyOpts{}
	fs.BoolVar(&o.start, "start", false, "start the background proxy if needed; print its base URL")
	fs.BoolVar(&o.stop, "stop", false, "stop the background proxy; stays down until the next start")
	fs.BoolVar(&o.restart, "restart", false, "stop the proxy if running, then start it; print its base URL")
	fs.BoolVar(&o.status, "status", false, "proxy pid, URL and the profile fallback chain; exit 1 when not running")
	fs.BoolVar(&o.serve, "serve", false, "run the proxy in the foreground until interrupted")
	fs.IntVar(&o.port, "port", proxy.DefaultPort, "with --serve: loopback port (a busy port held by an older proxy is reclaimed, else an error)")
	o.format = helptext.FormatFlag(fs)
	return fs, o
}

// check rejects stray args and flag combinations; ok is false when code is the exit status.
func (o *proxyOpts) check(fs *flag.FlagSet) (code int, ok bool) {
	if fs.NArg() > 0 {
		return helptext.Fail("gateway proxy: unexpected argument %q", fs.Arg(0)), false
	}
	if err := helptext.CheckFormat(*o.format); err != nil {
		return helptext.BadFormat("gateway proxy", err), false
	}
	return o.checkCombos(fs)
}

func (o *proxyOpts) modeCount() int {
	n := 0
	for _, b := range []bool{o.start, o.stop, o.restart, o.status, o.serve} {
		if b {
			n++
		}
	}
	return n
}

func (o *proxyOpts) checkCombos(fs *flag.FlagSet) (code int, ok bool) {
	if o.modeCount() > 1 {
		return helptext.Fail("gateway proxy: at most one of --start, --stop, --restart, --status, --serve"), false
	}
	portSet := false
	fs.Visit(func(f *flag.Flag) { portSet = portSet || f.Name == "port" })
	if portSet && !o.serve {
		return helptext.Fail("gateway proxy: --port needs --serve"), false
	}
	if *o.format == helptext.JSON && (o.start || o.stop || o.restart || o.serve) {
		return helptext.Fail("gateway proxy: --format json applies to --status only"), false
	}
	return 0, true
}

// cmdGatewayProxy is `gateway proxy`: at most one lifecycle flag, no flag = --status; stray args are errors.
func cmdGatewayProxy(args []string) int {
	fs, o := proxyFlags()
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if code, ok := o.check(fs); !ok {
		return code
	}
	ctx := context.Background()
	switch {
	case o.serve:
		return proxyServe(ctx, o.port)
	case o.start:
		return proxyStart(ctx)
	case o.restart:
		return proxy.Restart(ctx)
	case o.stop:
		return proxy.StopWithHold(ctx)
	}
	return proxyStatus(ctx, *o.format)
}

func proxyStart(ctx context.Context) int {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	base, err := proxy.Ensure(ctx)
	if err != nil {
		return helptext.Fail("proxy: %v", err)
	}
	fmt.Println(base)
	return 0
}

// proxyServe runs the proxy in the foreground until SIGINT/SIGTERM.
func proxyServe(ctx context.Context, port int) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Serve(ctx, port); err != nil {
		return helptext.Fail("proxy: %v", err)
	}
	return 0
}

func proxyStatus(ctx context.Context, format string) int {
	ps, err := proxy.LoadStatus(ctx)
	if err != nil {
		return helptext.Fail("%v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, ps); err != nil {
			return helptext.Fail("gateway proxy: %v", err)
		}
	} else if err := printProxyState(ps); err != nil {
		return helptext.Fail("gateway proxy: %v", err)
	}
	if !ps.Running {
		return 1
	}
	return 0
}
