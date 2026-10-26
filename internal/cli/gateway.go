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

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

var gatewayPage = helptext.Page{
	Usage: "claude-code gateway [command] [flags]",
	About: "Where a request goes: the runner pick (route) and the loopback proxy (proxy).\n" +
		"Bare: the route pick, then the proxy status.",
	Commands: [][2]string{
		{"route", "which runner + model a tier routes to, and why"},
		{"proxy", "the loopback router: --start | --stop | --status | --serve"},
	},
	Hint: "claude-code gateway <command> -h lists that command's flags.",
}

func cmdGateway(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			gatewayPage.Fprint(os.Stderr)
			return 0
		case "route":
			return cmdGatewayRoute(args[1:])
		case "proxy":
			return cmdGatewayProxy(args[1:])
		}
		if !strings.HasPrefix(args[0], "-") {
			return fail("gateway: unknown command %q (route|proxy)", args[0])
		}
	}
	return gatewayRoute(args, true)
}

// proxyState is the loopback proxy's status: the payload of `gateway proxy --format json` and bare `gateway`'s "proxy".
type proxyState struct {
	Running  bool     `json:"running"`
	PID      int      `json:"pid,omitempty"`
	URL      string   `json:"url,omitempty"`
	Started  string   `json:"started,omitempty"`
	Fallback []string `json:"fallback"`
	log      string
}

func loadProxyState(ctx context.Context) (proxyState, error) {
	ps := proxyState{log: router.LogFile()}
	if st, ok := router.Running(ctx); ok {
		ps.Running, ps.PID, ps.URL = true, st.PID, router.Base(st.Port)
		ps.Started = st.Started.Local().Format(time.RFC3339)
	}
	reg, err := providers.Load()
	if err != nil {
		return ps, err
	}
	ps.Fallback = append([]string{}, reg.Fallback...)
	return ps, nil
}

func printProxyState(ps proxyState) {
	if ps.Running {
		fmt.Printf("router    running  pid %d  %s  since %s\n", ps.PID, ps.URL, ps.Started)
	} else {
		fmt.Printf("router    not running  (log %s)\n", ps.log)
	}
	if len(ps.Fallback) == 0 {
		fmt.Println("fallback  none")
	} else {
		fmt.Println("fallback ", strings.Join(ps.Fallback, " → "))
	}
}

// cmdGatewayProxy is `gateway proxy`: at most one lifecycle flag, no flag = --status; stray args are errors.
func cmdGatewayProxy(args []string) int {
	fs := helptext.NewFlagSet("gateway proxy", helptext.Page{
		Usage: "claude-code gateway proxy [--start|--stop|--status|--serve] [flags]",
		About: "The loopback model router that translates Anthropic requests for OpenAI-style\n" +
			"providers and falls back across the registry's fallback chain. No flag = --status.",
	})
	start := fs.Bool("start", false, "start the background router if needed; print its base URL")
	stop := fs.Bool("stop", false, "stop the background router")
	status := fs.Bool("status", false, "router pid, URL and the provider fallback chain; exit 1 when not running")
	serve := fs.Bool("serve", false, "run the router in the foreground until interrupted")
	port := fs.Int("port", router.DefaultPort, "with --serve: loopback port (a busy port held by an older router is reclaimed, else an error)")
	format := outfmt.Flag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return fail("gateway proxy: unexpected argument %q", fs.Arg(0))
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("gateway proxy: %v", err)
	}
	n := 0
	for _, b := range []bool{*start, *stop, *status, *serve} {
		if b {
			n++
		}
	}
	if n > 1 {
		return fail("gateway proxy: at most one of --start, --stop, --status, --serve")
	}
	portSet := false
	fs.Visit(func(f *flag.Flag) { portSet = portSet || f.Name == "port" })
	if portSet && !*serve {
		return fail("gateway proxy: --port needs --serve")
	}
	if *format == outfmt.JSON && (*start || *stop || *serve) {
		return fail("gateway proxy: --format json applies to --status only")
	}
	ctx := context.Background()
	switch {
	case *serve:
		return proxyServe(ctx, *port)
	case *start:
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		base, err := router.Ensure(ctx)
		if err != nil {
			return fail("router: %v", err)
		}
		fmt.Println(base)
		return 0
	case *stop:
		st, err := router.Stop(ctx)
		if errors.Is(err, router.ErrNotRunning) {
			fmt.Println("router: not running")
			return 0
		}
		if err != nil {
			return fail("router: %v", err)
		}
		fmt.Printf("router: stopped pid %d\n", st.PID)
		return 0
	}
	return proxyStatus(ctx, *format)
}

// proxyServe runs the router in the foreground until SIGINT/SIGTERM.
func proxyServe(ctx context.Context, port int) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := router.Serve(ctx, port); err != nil {
		return fail("router: %v", err)
	}
	return 0
}

func proxyStatus(ctx context.Context, format string) int {
	ps, err := loadProxyState(ctx)
	if err != nil {
		return fail("%v", err)
	}
	if format == outfmt.JSON {
		if err := outfmt.Write(os.Stdout, ps); err != nil {
			return 1
		}
	} else {
		printProxyState(ps)
	}
	if !ps.Running {
		return 1
	}
	return 0
}
