package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
)

var routerPage = helptext.Page{
	Usage: "claude-code router <command>",
	About: "The loopback model router that translates Anthropic requests for OpenAI-style\n" +
		"providers and falls back across the registry's fallback chain.",
	Commands: [][2]string{
		{"serve", "run the router in the foreground (--port)"},
		{"start", "start the background router if needed; print its base URL"},
		{"stop", "stop the background router"},
		{"status", "router pid, URL and the provider fallback chain"},
	},
}

func cmdRouter(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		routerPage.Fprint(os.Stderr)
		return 0
	}
	ctx := context.Background()
	// start/stop/status take no arguments: -h prints usage, anything else is a usage error, never a silent start.
	if args[0] != "serve" && len(args) > 1 {
		if args[1] == "-h" || args[1] == "--help" {
			routerPage.Fprint(os.Stderr)
			return 0
		}
		return fail("router %s: unexpected argument %q", args[0], args[1])
	}
	switch args[0] {
	case "serve":
		return routerServe(ctx, args[1:])
	case "start":
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		base, err := router.Ensure(ctx)
		if err != nil {
			return fail("router: %v", err)
		}
		fmt.Println(base)
		return 0
	case "stop":
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
	case "status":
		return routerStatus(ctx)
	}
	return fail("router: unknown subcommand %q (serve|start|stop|status)", args[0])
}

// routerServe runs the router in the foreground until SIGINT/SIGTERM.
func routerServe(ctx context.Context, args []string) int {
	fs := helptext.NewFlagSet("router serve", helptext.Page{Usage: "claude-code router serve [flags]",
		About: "Run the loopback router in the foreground until interrupted."})
	port := fs.Int("port", router.DefaultPort, "loopback port (falls back to a free port when busy)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := router.Serve(ctx, *port); err != nil {
		return fail("router: %v", err)
	}
	return 0
}

func routerStatus(ctx context.Context) int {
	st, ok := router.Running(ctx)
	if ok {
		fmt.Printf("router    running  pid %d  %s  since %s\n", st.PID, router.Base(st.Port), st.Started.Local().Format(time.RFC3339))
	} else {
		fmt.Printf("router    not running  (log %s)\n", router.LogFile())
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("%v", err)
	}
	if len(reg.Fallback) == 0 {
		fmt.Println("fallback  none")
	} else {
		fmt.Println("fallback ", strings.Join(reg.Fallback, " → "))
	}
	if !ok {
		return 1
	}
	return 0
}
