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

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var gatewayPage = helptext.Page{
	Usage: "claude-code gateway [command] [flags]",
	About: "Where a request goes: the agent + model pick (router) and the loopback proxy\n" +
		"(proxy). Bare: the router pick, then the proxy status.",
	Commands: [][2]string{
		{"router", "which agent + model a task routes to, and why"},
		{"proxy", "the loopback proxy: --start | --stop | --status | --serve"},
		{"token", "print only the proxy credential for a credential helper"},
	},
	Hint: "claude-code gateway <command> -h lists that command's flags.",
}

func cmdGateway(args []string) int {
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
			return fail("gateway: unknown command %q (router|proxy|token)", args[0])
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
	ps := proxyState{log: proxy.LogFile()}
	if st, ok := proxy.Running(ctx); ok {
		ps.Running, ps.PID, ps.URL = true, st.PID, proxy.Base(st.Port)
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
		fmt.Printf("proxy     running  pid %d  %s  since %s\n", ps.PID, ps.URL, ps.Started)
	} else {
		fmt.Printf("proxy     not running  (log %s)\n", ps.log)
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
		About: "The loopback proxy that translates Anthropic requests for OpenAI-style\n" +
			"providers and falls back across the registry's fallback chain. No flag = --status.",
	})
	start := fs.Bool("start", false, "start the background proxy if needed; print its base URL")
	stop := fs.Bool("stop", false, "stop the background proxy")
	status := fs.Bool("status", false, "proxy pid, URL and the profile fallback chain; exit 1 when not running")
	serve := fs.Bool("serve", false, "run the proxy in the foreground until interrupted")
	port := fs.Int("port", proxy.DefaultPort, "with --serve: loopback port (a busy port held by an older proxy is reclaimed, else an error)")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return fail("gateway proxy: unexpected argument %q", fs.Arg(0))
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat("gateway proxy", err)
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
	if *format == helptext.JSON && (*start || *stop || *serve) {
		return fail("gateway proxy: --format json applies to --status only")
	}
	ctx := context.Background()
	switch {
	case *serve:
		return proxyServe(ctx, *port)
	case *start:
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		base, err := proxy.Ensure(ctx)
		if err != nil {
			return fail("proxy: %v", err)
		}
		fmt.Println(base)
		return 0
	case *stop:
		st, err := proxy.Stop(ctx)
		if errors.Is(err, proxy.ErrNotRunning) {
			fmt.Println("proxy: not running")
			return 0
		}
		if err != nil {
			return fail("proxy: %v", err)
		}
		fmt.Printf("proxy: stopped pid %d\n", st.PID)
		return 0
	}
	return proxyStatus(ctx, *format)
}

// proxyServe runs the proxy in the foreground until SIGINT/SIGTERM.
func proxyServe(ctx context.Context, port int) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// route imports the proxy package, so the auth-mark store is injected here rather than imported by it.
	proxy.Gate = proxy.AuthGate{Account: router.ProviderAccount, Mark: router.MarkAuth, Clear: router.ClearAuth,
		Marked: func() func(string, time.Time) (string, bool) { return router.LoadState().AuthMarked }}
	if err := proxy.Serve(ctx, port); err != nil {
		return fail("proxy: %v", err)
	}
	return 0
}

func proxyStatus(ctx context.Context, format string) int {
	ps, err := loadProxyState(ctx)
	if err != nil {
		return fail("%v", err)
	}
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, ps); err != nil {
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

var (
	tokenEnsure = proxy.Ensure
	tokenState  = proxy.Running
	tokenMint   = proxy.MintSessionToken
)

func cmdGatewayToken(args []string) int {
	fs := helptext.NewFlagSet("gateway token", helptext.Page{
		Usage: "claude-code gateway token --profile p",
		About: "Print a proxy session token for a credential helper (stdout only).\n" +
			"The token is bound to the proxy daemon and scoped to one profile.",
	})
	profile := fs.String("profile", "", "profile the token is scoped to (required)")
	cfg := fs.String("config-dir", "", "CLAUDE_CONFIG_DIR to use (native Windows helper)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() != 0 || *profile == "" {
		return fail("gateway token: want --profile <name> and no other arguments")
	}
	if *cfg != "" {
		_ = os.Setenv("CLAUDE_CONFIG_DIR", *cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := tokenEnsure(ctx); err != nil {
		return fail("gateway token: proxy unavailable; run gateway proxy --start")
	}
	st, ok := tokenState(ctx)
	if !ok {
		return fail("gateway token: proxy not running")
	}
	token, err := tokenMint(ctx, *profile, st.PID)
	if err != nil {
		return fail("gateway token: %v", err)
	}
	if _, err := fmt.Fprintln(os.Stdout, token); err != nil {
		return 1
	}
	return 0
}
