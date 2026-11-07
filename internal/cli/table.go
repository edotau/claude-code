package cli

import (
	"os"

	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/doctor"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/memory/memorycmd"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/network"
	"github.com/edotau/claude-code/internal/setup"
	"github.com/edotau/claude-code/internal/vault/vaultcmd"
	"github.com/edotau/claude-code/internal/worktree/worktreecmd"
)

// command is one top-level verb; group places it on the menu, hidden keeps it dispatchable but off the menu.
type command struct {
	name    string
	summary string // longer than a line wraps with a hanging indent
	group   string
	hidden  bool
	run     func(args []string) int
	spec    func() *completion.Node
}

// Menu groups; declaration order in commands() is the display order.
const (
	groupAnthropic   = "ANTHROPIC"
	groupAgents      = "AGENTS"
	groupAuth        = "AUTH"
	groupSession     = "SESSION"
	groupDiagnostics = "DIAGNOSTICS"
	groupSettings    = "SETTINGS"
)

// alphabetical groups list their verbs A→Z; ANTHROPIC (alias order) and AGENTS keep declaration order.
var alphabetical = map[string]bool{groupAuth: true, groupSession: true, groupDiagnostics: true, groupSettings: true}

func commands() []command {
	return append([]command{
		// AGENTS — launch an agent CLI; `ask` runs one task headlessly.
		{name: "opus", group: groupAnthropic, summary: "high reasoning: deep architecture · $$$", run: aliasVerb(models.Opus)},
		{name: "sonnet", group: groupAnthropic, summary: "everyday coding, reviews, subagents · $$", run: aliasVerb(models.Sonnet)},
		{name: "haiku", group: groupAnthropic, summary: "quick lookups, mechanical edits · $ fastest", run: aliasVerb(models.Haiku)},
		{name: "fable", group: groupAnthropic, summary: "separate frontier family; main-session default · $$$$", run: aliasVerb(models.Fable)},
		{name: "anthropic", group: groupAnthropic, hidden: true, summary: "first-party Claude: login, --oauth token or --x-api-key", run: cmdAnthropic},
		{name: "science", group: groupAnthropic, summary: "Claude for Life Sciences, own login (--print-env)", run: cmdScience},
		{name: "copilot", group: groupAgents, summary: "Copilot CLI on its own login, or your Databricks profile", run: func(a []string) int { return agentVerb("copilot", a) }},
		{name: "codex", group: groupAgents, summary: "Codex on your ChatGPT login; reviews", run: func(a []string) int { return agentVerb("codex", a) }},
		{name: "gemini", group: groupAgents, summary: "Gemini CLI; 1M-context sweeps (ask --dirs)", run: cmdGemini},
		{name: "opencode", group: groupAgents, summary: "OpenCode on its own login, or your Databricks profile; docs", run: func(a []string) int { return agentVerb("opencode", a) }},
		{name: "databricks", group: groupAgents, summary: "Claude Code on Databricks; login, import/export, check --all", run: databricks.Run},
		// AUTH
		{name: "resolve", group: groupAuth, hidden: true, summary: "resolve + print a profile's credential (apiKeyHelper)", run: cmdResolve},
		{name: "vault", group: groupAuth, summary: "pins, env, overlays and sealed secrets", run: vaultcmd.Run},
		{name: "network", group: groupAuth, summary: "tailnet, browsers, web search, MCP health, agent page", run: func(a []string) int { return network.Run(a, func() any { return verbTree() }) }, spec: networkSpecNodes},
		// SESSION
		{name: "switch", group: groupSession, hidden: true, summary: "pin the session profile (vault pins)", run: cmdSwitch},
		{name: "clean", group: groupSession, summary: "sweep accumulated state; --dry-run previews", run: func(args []string) int { return setup.Clean(args, os.Stdout, os.Stderr) }},
		{name: "history", group: groupSession, summary: "shadow git history of the config files", run: cmdHistory},
		{name: "worktree", group: groupSession, summary: "manage worktrees in ~/worktrees/<repo>", run: worktreecmd.Run, spec: worktreecmd.Spec},
		{name: "test", group: groupSession, hidden: true, summary: "test tooling: mutate checks a test has teeth", run: cmdTest},
		{name: "memory", group: groupSession, summary: "session memory bank: recall, save, promote", run: func(a []string) int { return memorycmd.Run(a, func(verb string) bool { return lookup(verb) != nil }) }, spec: memorycmd.Spec},
		{name: "plan", group: groupSession, hidden: true, summary: "plan checklists: status (ticked/total, claims)", run: cmdPlan},
		// DIAGNOSTICS
		{name: "doctor", group: groupDiagnostics, summary: "read-only health checks (--audit: action log)", run: doctor.Run, spec: doctor.SpecNode},
		{name: "gateway", group: groupDiagnostics, summary: "which agent + model a task routes to; the loopback proxy", run: gateway.Run, spec: gateway.Spec},
		{name: "usage", group: groupDiagnostics, summary: "token totals, peak rate, profile quotas", run: cmdUsage},
		{name: "describe", group: groupDiagnostics, summary: "machine-readable map of the harness", run: cmdDescribe},
		// SETTINGS
		{name: "install", group: groupSettings, summary: "install Claude Code; seed settings.json if missing", run: func(a []string) int { return launch.RunInstall(a, cmdSetup) }},
		{name: "configure", group: groupSettings, summary: "guided setup; --restore resets settings.json", run: cmdConfigure},
		{name: "theme", group: groupSettings, summary: "palette, statusline, VS Code template, tool themes", run: cmdTheme},
	}, hiddenCommands()...)
}

func hiddenCommands() []command {
	return []command{
		// Hidden: dispatchable, named in the trailing note, off the menu.
		{name: "claude", hidden: true, summary: "launch Claude Code on the selected profile", run: cmdClaude},
		{name: "help-svg", hidden: true, summary: "render this menu as the README SVG (--out f, --check f)", run: cmdMenuSVG},
		{name: "agents", hidden: true, summary: "ask, dispatch, list, sync, tree, status: run and track agents", run: CmdAgents},
		{name: "preflight", hidden: true, summary: "pin check per agent; --live runs the post-install pong round", run: doctor.Preflight, spec: doctor.PreflightSpecNode},
		{name: "models", hidden: true, summary: "model aliases; --pin alias=model, --live", run: cmdModels},
		{name: "review", hidden: true, summary: "code-review detectors: quality|complexity|diff|pr|gate", run: cmdReview},
		{name: "workflow", hidden: true, summary: "skeleton <pattern> | lint (model/effort pins, meta/throw)", run: cmdWorkflow},
		{name: "hook", hidden: true, summary: "run a lifecycle hook (invoked by settings.json)", run: cmdHook},
		{name: "clip", hidden: true, summary: "save the Windows clipboard image as PNG; print its WSL path", run: cmdClip},
		{name: "clip-clear", hidden: true, summary: "clear the clipboard after a delay if it still holds a copied secret", run: cmdClipClear},
		{name: "wl-paste", hidden: true, summary: "WSL wl-paste proxy: answer agent image paste, else run real wl-paste", run: cmdWLPaste},
		{name: "version", hidden: true, summary: "print the build version", run: cmdVersion},
		{name: "completion", hidden: true, summary: "bash tab-completion script (--install to write it)", run: cmdCompletion},
		{name: "__complete", hidden: true, summary: "machine: compute tab-completion candidates", run: cmdComplete},
	}
}
