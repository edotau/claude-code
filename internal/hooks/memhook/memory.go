package memhook

import (
	"os"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
)

// Start injects the memory bank (auto-scaffolding it for safe roots) as additionalContext.
func Start(in *core.Input) core.Result {
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	ctx := memory.StartSession(in.SessionID, cwd, time.Now())
	refreshServed(cwd, time.Now())
	res := core.Result{Context: core.JoinContext(ctx, preflightNotice(time.Now()))}
	if !memory.IsUpdateChild() {
		res.SystemMessage = memory.ConsumeCatchupNote()
	}
	if in.Source == "compact" || in.Source == "clear" {
		if note := memory.ResumeNote(in.Source, in.SessionID, cwd, time.Now()); note != "" {
			res.SystemMessage = joinMessages(res.SystemMessage, note)
			res.Context = core.JoinContext(res.Context, note)
		}
	}
	return res
}

// preflightNotice is the static pin check (cached listings only): one short line when something needs a fix.
func preflightNotice(now time.Time) string {
	reg, err := providers.Load()
	if err != nil {
		return ""
	}
	return preflight.StaticLine(reg, now)
}

// refreshServed re-lists a stale served-endpoint cache detached; needs no memory bank, never from an update child.
func refreshServed(cwd string, now time.Time) {
	if !memory.IsUpdateChild() && servedRefreshDue(now) {
		spawnServed(cwd)
	}
}

// servedRefreshDue reports whether served routing is on and an in-use Databricks profile has no fresh listing.
func servedRefreshDue(now time.Time) bool {
	if !providers.ServedRoutingOn() {
		return false
	}
	reg, err := providers.Load()
	return err == nil && preflight.Stale(reg, now) && preflight.Claim(now)
}

// spawnServed is the detached `preflight --refresh` seam; tests replace it.
var spawnServed = func(cwd string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	defer null.Close()
	return proc.SpawnDetached(self, []string{"preflight", "--refresh"}, cwd, null)
}
