// Package hooks implements `claude-code hook <Event> [--<module>|--all]`: JSON on stdin, exit 0 = proceed, 2 = block.
// Hooks fail OPEN — an infra glitch never blocks the session.
package hooks

import (
	"fmt"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/memory"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
)

// Native maps every hook module to its Runner; names must match settings.Registry.
var Native = map[string]core.Runner{
	"safety-scan":               safetyScan,
	"rtk-rewrite":               rtkRewrite,
	"docs-placement":            viaStderr(docs.Placement),
	"memory-start":              memory.Start,
	"memory-recall":             memory.Recall,
	"paths-resolve":             docs.ResolvePaths,
	"memory-update":             memory.Stop,
	"review-simplicity":         reviewSimplicity,
	"format-stop":               stopFormatRun,
	"memory-end":                memory.End,
	"dispatch-effort":           dispatchPre,
	"codex-agent-route":         nativeCodexAgentRoute,
	"codex-routing-start":       nativeCodexRoutingStart,
	"codex-routing-result":      nativeCodexRoutingResult,
	"codex-subagent-start":      nativeCodexSubagentStart,
	"dispatch-failure":          releaseSpawn,
	"dispatch-workflow":         releaseSpawn,
	"docs-plan-gate":            viaStderr(docs.Gate),
	"docs-plan-skeleton":        docs.Skeleton,
	"docs-plan-skeleton-prompt": docs.Skeleton,
	"docs-plan-tick":            docs.Tick,
	"docs-plan-reminder":        docs.Reminder,
	"docs-plan-mirror":          viaStderr(docs.Mirror),
	"docs-plan-save":            viaStderr(docs.Save),
	"dispatch-stop":             subagentStopRunner,
	"memory-compact":            memory.Compact,
	"worktree-create":           worktreeCreate,
	"worktree-remove":           viaStderr(worktreeRemove),
	"worktree-mode":             worktreeMode,
	"nested-config-sweep":       nestedConfigSweep,
	"worktree-busy":             worktreeBusy,
	"worktree-claim":            worktreeClaim,
	"worktree-claim-check":      worktreeClaimCheck,
}

// runDirect is `hook <Event> --<module>`: one Runner rendered for its own process. A module that blocks exits 2 with
// its reason on stderr, which is how a guard run by an agent or by safety-scan's own entry blocks.
func runDirect(name string, stdin io.Reader, stdout, stderr io.Writer) int {
	run, ok := Native[name]
	if !ok {
		return core.ExitProceed
	}
	in := core.ParseInput(stdin)
	res := run(in)
	if res.Decision == core.Block && specOf(name).Blocks {
		if name == "safety-scan" && in.Agent() == core.AgentCopilot {
			fmt.Fprintln(stdout, core.OutputCopilot(settings.EventPreToolUse, res))
		}
		fmt.Fprintln(stderr, res.Reason)
		return core.ExitBlock
	}
	res.Context = textutil.CapRunes(res.Context, core.ContextCapChars, "…")
	if !bareStdout[name] {
		return core.Emit(in.Agent(), eventOf(name), res, stdout, stderr)
	}
	var out strings.Builder
	code := core.Emit(in.Agent(), eventOf(name), res, &out, stderr)
	_, _ = io.WriteString(stdout, strings.TrimSuffix(out.String(), "\n"))
	return code
}

// bareStdout lists the modules whose own-process stdout never carried a trailing newline.
var bareStdout = map[string]bool{
	"review-simplicity": true, "docs-plan-skeleton": true, "docs-plan-skeleton-prompt": true,
	"docs-plan-tick": true, "docs-plan-reminder": true,
}

// specOf is the hookspec entry for a module; the zero spec when unknown.
func specOf(name string) settings.EventSpec {
	for _, s := range settings.Specs() {
		if s.Name == name {
			return s
		}
	}
	return settings.EventSpec{}
}

// eventOf names the hookspec event a handler is declared on.
func eventOf(name string) string { return specOf(name).Event }

// RunArgs is `hook <Event> [--<module>|--all]`; an unknown event or module fails closed only when it reads as a guard.
func RunArgs(event string, flags []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if event == CopilotBashHook {
		return copilotBash(stdin, stderr)
	}
	if !IsEvent(event) {
		return unknownHook(event, "unknown hook event", stderr)
	}
	switch {
	case len(flags) == 0:
		return RunEvent(event, false, stdin, stdout, stderr)
	case len(flags) == 1 && flags[0] == settings.AllFlag:
		return RunEvent(event, true, stdin, stdout, stderr)
	case len(flags) == 1 && strings.HasPrefix(flags[0], "--"):
		return runModule(event, strings.TrimPrefix(flags[0], "--"), stdin, stdout, stderr)
	}
	fmt.Fprintf(stderr, "claude-code hook %s: want no flag, --all or one --<module>, got %q\n", event, flags)
	return 1
}

// runModule runs one module on its declared event; a group switched off in harnessHooks proceeds.
func runModule(event, name string, stdin io.Reader, stdout, stderr io.Writer) int {
	if _, ok := Native[name]; !ok || eventOf(name) != event {
		return unknownHook(name, "no module on "+event+" named", stderr)
	}
	if g, found := settings.GroupOf(name); found && !enabledGroups()(g.Name, g.Gate) {
		return core.ExitProceed
	}
	return runDirect(name, stdin, stdout, stderr)
}

// unknownHook fails closed when the unknown name reads as a guard; a stale agent hook file must not drop its gate.
func unknownHook(name, what string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "claude-code hook: %s %q\n", what, name)
	for _, w := range []string{"guard", "gate", "safety", "scan", "placement"} {
		if strings.Contains(name, w) {
			return core.ExitBlock
		}
	}
	return 1
}
