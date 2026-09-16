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
	"github.com/edotau/claude-code/internal/router"
)

func cmdRouter(args []string) int {
	if len(args) == 0 {
		return fail("usage: claude-code router serve [--port N] | start | stop | status")
	}
	ctx := context.Background()
	switch args[0] {
	case "serve":
		fs := flag.NewFlagSet("router serve", flag.ContinueOnError)
		port := fs.Int("port", router.DefaultPort, "loopback port (the default falls back to a free port when busy)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := router.Serve(ctx, *port); err != nil {
			return fail("router: %v", err)
		}
		return 0
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

func routerClientSecret() (string, error) { return router.ClientSecret() }
