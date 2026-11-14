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
	cwd := cwdOf(in)
	now := time.Now()
	ctx := memory.StartSession(in.SessionID, cwd, now)
	reg, err := loadProviders() // one registry load serves the served refresh and the pin check
	if err != nil {
		reg = nil
	}
	pins := pinsOf(reg, now)
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
	now := time.Now()
	reg, err := loadProviders()
	if err != nil {
		reg = nil
	}
	return core.Result{Context: core.JoinContext(memory.SessionText(in.SessionID, cwdOf(in), now), preflightNotice(reg, pinsOf(reg, now)))}
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
func pinsOf(reg *providers.Registry, now time.Time) []preflight.Pin {
	if reg == nil {
		return nil
	}
	return preflight.Check(reg, now)
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

// loadProviders reads the provider registry (vault included); tests count it.
var loadProviders = providers.Load

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

func Recall(in *core.Input) core.Result {
	if core.AutomatedDelivery(in.Prompt) {
		return core.Result{}
	}
	return core.Result{Context: memory.PromptRecall(in.SessionID, in.CWD, in.Prompt, os.Stderr)}
}

// AgentInput returns in with TranscriptPath pointing at a Claude-shaped transcript synthesized from an opencode
// export, a Copilot events.jsonl or a codex rollout; any other input, or a failed load, comes back unchanged.
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
