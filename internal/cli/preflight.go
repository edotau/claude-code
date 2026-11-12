package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

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
func preflightLive(reg *providers.Registry, profile string) int {
	bin, err := os.Executable()
	if err != nil {
		return helptext.Fail("preflight: %v", err)
	}
	var pongs []preflight.Pong
	if profile != "" {
		pongs = preflight.RunProfilePongs(context.Background(), os.Stdout, reg, bin, profile)
		if len(pongs) == 0 {
			return helptext.Fail("preflight: no pong ran for %q (unknown, non-Databricks or no credential)", profile)
		}
	} else {
		pongs = preflight.RunInstallStep(context.Background(), os.Stdout, reg, bin, time.Now())
		if len(pongs) == 0 && pinsBroken(reg) { // a vault error skips every pong; that must not exit 0
			return helptext.Fail("preflight: no pong ran: the pin check failed (see the fix rows above)")
		}
	}
	if slices.ContainsFunc(pongs, func(p preflight.Pong) bool { return !p.OK }) {
		return 1
	}
	return 0
}

var preflightPage = helptext.Page{
	Usage: "claude-code preflight [--live [--profile <p>|all]] [flags]\nclaude-code preflight --refresh|--discover <profile>",
	About: "Print each agent's pin check; --live also refreshes stale listings and runs the post-install pong round.\n" +
		"--refresh only re-lists stale listings, silently.\n" +
		"--discover re-seals one profile's model snapshot (Databricks or github), silently (a launch spawns it when expired).",
}

// cmdPreflight is the hidden `preflight [--live|--refresh]`: pin rows, the post-install step, or a silent re-list.
func cmdPreflight(args []string) int {
	fs := helptext.NewFlagSet("preflight", preflightPage)
	live := fs.Bool("live", false, "refresh stale listings and ask each pinned agent for pong")
	profile := fs.String("profile", "", "with --live: pong every agent on this profile, or all Databricks profiles (all)")
	refresh := fs.Bool("refresh", false, "re-list stale served-endpoint caches, print nothing")
	discover := fs.String("discover", "", "re-seal one profile's model snapshot (Databricks or github)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if msg := preflightConflict(*discover, *live, *refresh, *profile); msg != "" {
		return helptext.Fail("preflight: %s", msg)
	}
	if *discover != "" {
		return preflightDiscover(*discover)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("preflight: %v", err)
	}
	return preflightReport(reg, *refresh, *live, *profile)
}

// pinsBroken reports whether the pin check hit an unreadable vault (a StatusError row).
func pinsBroken(reg *providers.Registry) bool {
	return slices.ContainsFunc(preflight.Check(reg, time.Now()), func(p preflight.Pin) bool { return p.Status == preflight.StatusError })
}

// preflightConflict names the flag combination that would silently drop a flag, or "" when valid.
func preflightConflict(discover string, live, refresh bool, profile string) string {
	switch {
	case discover != "" && (live || refresh || profile != ""):
		return "--discover cannot be combined with --live, --refresh or --profile"
	case profile != "" && !live:
		return "--profile requires --live"
	case refresh && live:
		return "--refresh cannot be combined with --live"
	}
	return ""
}

// preflightReport runs the refresh or live step when asked, else prints each agent's pin check.
func preflightReport(reg *providers.Registry, refresh, live bool, profile string) int {
	if refresh {
		preflightRefresh(reg)
		return 0
	}
	if live {
		return preflightLive(reg, profile)
	}
	for _, p := range preflight.Check(reg, time.Now()) {
		fmt.Printf("%-8s %-9s %s\n", p.Agent, p.Status, p.Line())
	}
	return 0
}

// preflightSpec is the `preflight` completion spec.
func preflightSpec() *helptext.Node {
	return &helptext.Node{Flags: []helptext.Flag{{Name: "discover"}, {Name: "live", IsBool: true}, {Name: "profile"}, {Name: "refresh", IsBool: true}}}
}
