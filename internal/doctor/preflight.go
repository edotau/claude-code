package doctor

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// preflightRefresh re-lists stale served-endpoint caches within the preflight budget, silently.
func preflightRefresh(reg *providers.Registry) {
	now := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), preflight.Budget)
	defer cancel()
	preflight.RefreshStale(ctx, reg, preflight.Check(reg, now), now, false)
}

// preflightDiscover re-seals one profile's model snapshot within the preflight budget.
func preflightDiscover(profile string) int {
	ctx, cancel := context.WithTimeout(context.Background(), preflight.Budget)
	defer cancel()
	if err := copilot.RefreshProfile(ctx, profile); err != nil {
		return helptext.Fail("preflight: %v", err)
	}
	return 0
}

// preflightLive runs the post-install pong round against the running binary.
func preflightLive(reg *providers.Registry) int {
	bin, err := os.Executable()
	if err != nil {
		return helptext.Fail("preflight: %v", err)
	}
	preflight.RunInstallStep(context.Background(), os.Stdout, reg, bin, time.Now())
	return 0
}

// Preflight is the hidden `preflight [--live|--refresh]`: pin rows, the post-install step, or a silent re-list.
func Preflight(args []string) int {
	fs := helptext.NewFlagSet("preflight", helptext.Page{
		Usage: "claude-code preflight [--live|--refresh|--discover <profile>]",
		About: "Print each agent's pin check; --live also refreshes stale listings and runs the post-install pong round.\n" +
			"--refresh only re-lists stale listings, silently.\n" +
			"--discover re-seals one profile's model snapshot (Databricks or github), silently (a launch spawns it when expired).",
	})
	live := fs.Bool("live", false, "refresh stale listings and ask each pinned agent for pong")
	refresh := fs.Bool("refresh", false, "re-list stale served-endpoint caches, print nothing")
	discover := fs.String("discover", "", "re-seal one profile's model snapshot (Databricks or github)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *discover != "" {
		return preflightDiscover(*discover)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("preflight: %v", err)
	}
	return preflightReport(reg, *refresh, *live)
}

// preflightReport runs the refresh or live step when asked, else prints each agent's pin check.
func preflightReport(reg *providers.Registry, refresh, live bool) int {
	if refresh {
		preflightRefresh(reg)
		return 0
	}
	if live {
		return preflightLive(reg)
	}
	for _, p := range preflight.Check(reg, time.Now()) {
		fmt.Printf("%-8s %-9s %s\n", p.Agent, p.Status, p.Line())
	}
	return 0
}

// PreflightSpecNode is the `preflight` completion spec.
func PreflightSpecNode() *completion.Node {
	return &completion.Node{Flags: []completion.Flag{{Name: "discover"}, {Name: "live", IsBool: true}, {Name: "refresh", IsBool: true}}}
}
