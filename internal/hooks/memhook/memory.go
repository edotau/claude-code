package memhook

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/preflight"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/worktree"
)

// Start injects the memory bank (auto-scaffolding it for safe roots) as additionalContext.
func Start(in *core.Input) core.Result {
	hctx, cancel := core.HookContext("memory-start")
	defer cancel()
	cwd := cwdOf(in)
	now := time.Now()
	ctx := memory.StartSession(in.SessionID, cwd, now)
	reg := loadRegistry() // one registry load serves the served refresh and the pin check
	pins := pinsOf(hctx, reg, now)
	refreshServed(reg, pins, cwd, now)
	res := core.Result{Context: core.JoinContext(ctx, preflightNotice(reg, pins))}
	if !memory.IsUpdateChild() {
		res.SystemMessage = memory.ConsumeCatchupNote()
	}
	if in.Source == "compact" || in.Source == "clear" {
		if note := memory.ResumeNote(in.Source, in.SessionID, cwd, time.Now()); note != "" {
			res.SystemMessage = core.JoinNonEmpty("; ", res.SystemMessage, note)
			res.Context = core.JoinContext(res.Context, note)
		}
	}
	return res
}

// StartText is Start's context for an audit (doctor --prompts): nothing is written, spawned or consumed.
func StartText(in *core.Input) core.Result {
	ctx, cancel := core.HookContext("memory-start")
	defer cancel()
	now := time.Now()
	reg := loadRegistry()
	return core.Result{Context: core.JoinContext(memory.SessionText(in.SessionID, cwdOf(in), now), preflightNotice(reg, pinsOf(ctx, reg, now)))}
}

// cwdOf is the hook payload's cwd, else the process's.
func cwdOf(in *core.Input) string {
	if in.CWD != "" {
		return in.CWD
	}
	cwd, _ := os.Getwd()
	return cwd
}

// preflightNotice is the static pin check (cached listings only): one short line when something needs a fix.
func preflightNotice(reg *providers.Registry, pins []preflight.Pin) string {
	if reg == nil {
		return ""
	}
	return preflight.StaticLine(reg, pins)
}

// pinsOf runs the pin check once per start; nil without a registry.
func pinsOf(ctx context.Context, reg *providers.Registry, now time.Time) []preflight.Pin {
	if reg == nil {
		return nil
	}
	return preflight.Check(ctx, reg, now)
}

// refreshServed re-lists a stale served-endpoint cache detached; needs no memory bank, never from an update child.
func refreshServed(reg *providers.Registry, pins []preflight.Pin, cwd string, now time.Time) {
	if !memory.IsUpdateChild() && servedRefreshDue(reg, pins, now) {
		spawnServed(cwd)
	}
}

// servedRefreshDue reports whether served routing is on and an in-use Databricks profile has no fresh listing.
func servedRefreshDue(reg *providers.Registry, pins []preflight.Pin, now time.Time) bool {
	if !providers.ServedRoutingOn() {
		return false
	}
	return reg != nil && preflight.Stale(reg, pins, now) && preflight.Claim(now)
}

// loadRegistry is the provider registry, nil when it cannot load.
func loadRegistry() *providers.Registry {
	reg, err := loadProviders()
	if err != nil {
		return nil
	}
	return reg
}

// loadProviders reads the provider registry (vault included); tests count it.
var loadProviders = providers.Load

// spawnServed is the detached `models preflight --refresh` seam; tests replace it.
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
	return proc.SpawnDetached(self, []string{"models", "preflight", "--refresh"}, cwd, null)
}

func Recall(in *core.Input) core.Result {
	if core.AutomatedDelivery(in.Prompt) {
		return core.Result{}
	}
	ctx, cancel := core.HookContext("memory-recall")
	defer cancel()
	return core.Result{Context: memory.PromptRecall(ctx, in.SessionID, in.Prompt, os.Stderr)}
}

// AgentInput returns in with TranscriptPath set to a synthesized Claude-shaped transcript; others pass unchanged.
func AgentInput(in *core.Input) *core.Input {
	kind := agentKind(in)
	if kind == "" {
		return in
	}
	path, ok := memory.SynthAgentTranscript(kind, in.SessionID, in.CWD, in.TranscriptPath)
	if !ok {
		return in
	}
	out := *in
	out.TranscriptPath = path
	return &out
}

// agentKind names the memory source whose transcript AgentInput synthesizes, or "" for a Claude-shaped input.
func agentKind(in *core.Input) string {
	switch {
	case in.Agent() == core.AgentOpencode:
		return memory.SourceOpencode
	case in.Agent() == core.AgentCopilot && in.TranscriptPath != "":
		return memory.SourceCopilot
	case in.Agent() == core.AgentCodex && in.TranscriptPath != "":
		return memory.SourceCodex
	}
	return ""
}

// updateNotSaved is the visible line when the Stop hook proceeds without an update.
const updateNotSaved = "memory update NOT saved; run `claude-code memory update`"

// sessionUpdate spawns a detached update; a failed spawn proceeds with a note and SessionEnd/PreCompact retry.
func sessionUpdate(in *core.Input) (notSaved bool) {
	memory.RefreshStaleEmbeddings(in.CWD)
	if !memory.Updatable(in.SessionID, in.TranscriptPath) {
		return false
	}
	g := memory.UpdateIfNew(updateRequest(in), memory.TriggerStop, time.Now())
	return g.FailedOpen || g.Blocked
}

// End (SessionEnd) updates the final state when total work crosses the floor and exceeds the last dispatch's count.
func End(in *core.Input) core.Result {
	in = AgentInput(sessionRootInput(in))
	memory.RefreshStaleEmbeddings(in.CWD)
	if !memory.Updatable(in.SessionID, in.TranscriptPath) {
		return core.Result{}
	}
	memory.UpdateIfNew(updateRequest(in), memory.TriggerSessionEnd, time.Now())
	return core.Result{}
}

// updateRequest is in's update ask; each dispatch logs to the session's linked-worktree ledger.
func updateRequest(in *core.Input) memory.UpdateRequest {
	return memory.UpdateRequest{SessionID: in.SessionID, Transcript: in.TranscriptPath, CWD: in.CWD,
		OnDispatch: func(trigger string, work int, now time.Time) {
			logUpdateToLedger(in.CWD, in.SessionID, trigger, work, now)
		}}
}

// logUpdateToLedger notes an update in the session's linked-worktree ledger; best effort, never fatal.
func logUpdateToLedger(cwd, sessionID, trigger string, work int, now time.Time) {
	_, _ = worktree.AppendLedger(cwd, fmt.Sprintf("update (%s): %d edits, session %s", trigger, work, sessionID), now)
}

// sessionRoot reads the session's recorded root; tests count it.
var sessionRoot = memory.SessionRoot

func sessionRootInput(in *core.Input) *core.Input {
	copy := *in
	copy.CWD = sessionRoot(in.SessionID, in.CWD)
	return &copy
}
