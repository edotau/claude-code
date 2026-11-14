package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/waves"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// registerDispatchWaves registers the plan runner's options on dispatch's existing flag set.
func registerDispatchWaves(fs *flag.FlagSet) *waves.Options {
	o := &waves.Options{Out: os.Stdout, Err: os.Stderr}
	fs.StringVar(&o.Plan, "waves", "", "run the waves of this plan")
	fs.Func("wave", "wave N or inclusive range N-M (default: all)", func(s string) error {
		bounds := strings.Split(s, "-")
		if len(bounds) > 2 {
			return fmt.Errorf("invalid wave range %q", s)
		}
		var err error
		o.From, err = dispatchWaveNumber(bounds[0])
		if err != nil || o.From < 1 {
			return fmt.Errorf("invalid wave range %q", s)
		}
		o.To = o.From
		if len(bounds) == 2 {
			o.To, err = dispatchWaveNumber(bounds[1])
		}
		if err != nil || o.To < o.From {
			return fmt.Errorf("invalid wave range %q", s)
		}
		return nil
	})
	fs.BoolVar(&o.DryRun, "dry-run", false, "with --waves, list packages without launching")
	fs.BoolVar(&o.Status, "status", false, "with --waves, print saved state without launching")
	fs.BoolVar(&o.Fresh, "fresh", false, "with --waves, discard saved state and use current HEAD")
	fs.StringVar(&o.Gate, "gate", "", "with --waves, override the wave gate command")
	fs.StringVar(&o.Provider, "provider", "", "with --waves, provider for every leaf")
	for _, name := range []string{"profile", "profiles"} {
		helptext.ListVar(fs, &o.Profiles, name, "PROFILE", "with --waves, leaf profiles (repeatable or comma-separated)", true)
	}
	return o
}

// dispatchWaveNumber parses a decimal wave number without accepting trailing text.
func dispatchWaveNumber(s string) (int, error) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, fmt.Errorf("invalid wave number %q", s)
	}
	var n int
	_, err := fmt.Sscan(s, &n)
	return n, err
}

type waveJob struct {
	background bool
	id         string
	args       []string
}

// dispatchWaves preserves the runner's exit codes and forwards shared route flags to every leaf.
func dispatchWaves(pos []string, route *ask.RouteFlags, o *waves.Options, job waveJob) int {
	if job.background && (o.DryRun || o.Status || job.id != "") {
		return helptext.FailUsage("agents dispatch: --background cannot combine with --dry-run, --status or --job")
	}
	if len(pos) != 0 {
		return helptext.FailUsage("agents dispatch: --waves accepts no role or task argument")
	}
	var err error
	o.Route, err = route.Args()
	if err != nil {
		return helptext.FailUsage("agents dispatch: %v", err)
	}
	o.Dir, err = os.Getwd()
	if err != nil {
		return helptext.Fail("agents dispatch: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if job.background {
		return waves.Background(ctx, *o, job.args)
	}
	if job.id != "" {
		return waves.RunJob(ctx, *o, job.id)
	}
	return waves.Run(ctx, *o)
}
