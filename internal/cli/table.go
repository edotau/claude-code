package cli

import (
	"fmt"
	"os"
	"sync"

	"github.com/edotau/claude-code/internal/agent/agentcmd"
	"github.com/edotau/claude-code/internal/clipboard"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/describe"
	"github.com/edotau/claude-code/internal/doctor"
	"github.com/edotau/claude-code/internal/history"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/network"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/setup"
	"github.com/edotau/claude-code/internal/ui/helptext"
	usagecmd "github.com/edotau/claude-code/internal/usage"
)

// command is one top-level verb; group places it on the menu, hidden keeps it dispatchable but off the menu.
type command struct {
	name    string
	summary string // longer than a line wraps with a hanging indent
	group   string
	hidden  bool
	run     func(args []string) int
	spec    func() *helptext.Node
}

// Menu groups; declaration order in commands() is the display order.
const (
	groupAgents      = "AGENTS"
	groupAuth        = "AUTH"
	groupSession     = "SESSION"
	groupDiagnostics = "DIAGNOSTICS"
	groupSettings    = "SETTINGS"
)

// alphabetical groups list their verbs A→Z; AGENTS keeps declaration order.
var alphabetical = map[string]bool{groupAuth: true, groupSession: true, groupDiagnostics: true, groupSettings: true}

func commands() []command {
	return append([]command{
		// AGENTS — launch an agent CLI; `ask` runs one task headlessly.
		{name: "anthropic", group: groupAgents, summary: "Claude Code: --model opus|sonnet|haiku|fable; science", run: agentcmd.Anthropic, spec: agentcmd.AnthropicSpec},
		{name: "copilot", group: groupAgents, summary: "Copilot CLI on its own login, or your Databricks profile", run: func(a []string) int { return agentcmd.AgentVerb("copilot", a) }, spec: agentcmd.AgentSpec},
		{name: "codex", group: groupAgents, summary: "Codex on your ChatGPT login; reviews", run: func(a []string) int { return agentcmd.AgentVerb("codex", a) }, spec: agentcmd.AgentSpec},
		{name: "gemini", group: groupAgents, summary: "Gemini CLI; 1M-context sweeps (ask --dirs)", run: func(a []string) int { return agentcmd.AgentVerb("gemini", a) }, spec: agentcmd.AgentSpec},
		{name: "opencode", group: groupAgents, summary: "OpenCode on its own login, or your Databricks profile; docs", run: func(a []string) int { return agentcmd.AgentVerb("opencode", a) }, spec: agentcmd.AgentSpec},
		{name: "databricks", group: groupAgents, summary: "Claude Code on Databricks; login, import/export, check --all", run: databricks.Run, spec: databricks.Spec},
		// AUTH
		{name: "resolve", group: groupAuth, hidden: true, summary: "resolve + print a profile's credential (apiKeyHelper)", run: cmdResolve, spec: resolveSpec},
		{name: "vault", group: groupAuth, summary: "pins, env, overlays and sealed secrets", run: cmdVault, spec: vaultSpec},
		{name: "network", group: groupAuth, summary: "tailnet, browsers, web search, remote agents", run: network.Run, spec: network.Spec},
		// SESSION
		{name: "switch", group: groupSession, hidden: true, summary: "pin the session profile (vault pins)", run: cmdSwitch, spec: switchSpec},
		{name: "clean", group: groupSession, summary: "sweep accumulated state; --dry-run previews", run: func(args []string) int { return setup.Clean(args, os.Stdout, os.Stderr) }, spec: setup.CleanSpec},
		{name: "history", group: groupSession, summary: "shadow git history of the config files", run: history.Run, spec: history.SpecNode},
		{name: "worktree", group: groupSession, summary: "manage worktrees in ~/worktrees/<repo>", run: cmdWorktree, spec: worktreeSpec},
		{name: "test", group: groupSession, hidden: true, summary: "test tooling: mutate checks a test has teeth", run: cmdTest, spec: testSpec},
		{name: "memory", group: groupSession, summary: "session memory bank: recall, save, promote", run: func(a []string) int { return cmdMemory(a, func(verb string) bool { return lookup(verb) != nil }) }, spec: memorySpec},
		{name: "plan", group: groupSession, hidden: true, summary: "plan checklists: status (ticked/total, claims)", run: plans.Run, spec: plans.SpecNode},
		{name: "scm", group: groupSession, summary: "PRs, checks, comments on GitHub or Bitbucket DC", run: cmdScm, spec: scmSpec},
		// DIAGNOSTICS
		{name: "doctor", group: groupDiagnostics, summary: "read-only health checks (--audit: action log)", run: doctor.Run, spec: doctor.SpecNode},
		{name: "gateway", group: groupDiagnostics, summary: "which agent + model a task routes to; the loopback proxy", run: cmdGateway, spec: gatewaySpec},
		{name: "usage", group: groupDiagnostics, summary: "token totals, peak rate, profile quotas", run: func(a []string) int { return usagecmd.Run(a, paths.ConfigDir(), os.Stdout, os.Stderr) }, spec: usagecmd.Spec},
		// SETTINGS
		{name: "install", group: groupSettings, summary: "install Claude Code; seed settings.json if missing", run: func(a []string) int { return launch.RunInstall(a, setup.Run) }, spec: setup.InstallSpec},
		{name: "configure", group: groupSettings, summary: "guided setup; --restore resets settings.json", run: func(a []string) int {
			return cmdConfigure(a, func(name, format string) int { return providerList(name, format, describeVerbs) })
		}, spec: configureSpec},
		{name: "theme", group: groupSettings, summary: "palette, statusline, VS Code template, tool themes", run: cmdTheme, spec: themeSpec},
	}, hiddenCommands()...)
}

// describeVerbs lists the table's verbs with their completion specs, for describe.
func describeVerbs() []describe.Verb {
	tree := specTree()
	var out []describe.Verb
	for _, c := range commands() {
		out = append(out, describe.Verb{Name: c.name, Group: c.group, Summary: c.summary, Hidden: c.hidden, Node: tree[c.name]})
	}
	return out
}

func hiddenCommands() []command {
	return []command{
		// Hidden: dispatchable, named in the trailing note, off the menu.
		{name: "claude", hidden: true, summary: "launch Claude Code on the selected profile", run: agentcmd.Claude},
		{name: "help-svg", hidden: true, summary: "render this menu as the README SVG (--out f, --check f)", run: cmdMenuSVG, spec: menuSVGSpec},
		{name: "agents", hidden: true, summary: "ask, dispatch, list, sync, tree, status: run and track agents", run: agentcmd.Run, spec: agentcmd.AgentsSpec},
		{name: "preflight", hidden: true, summary: "pin check per agent; --live runs the post-install pong round", run: doctor.Preflight, spec: doctor.PreflightSpecNode},
		{name: "models", hidden: true, summary: "model aliases; --pin alias=model, --live", run: cmdModels, spec: modelsSpec},
		{name: "review", hidden: true, summary: "code-review detectors: quality|complexity|diff|pr|gate", run: cmdReview, spec: reviewSpec},
		{name: "workflow", hidden: true, summary: "skeleton <pattern> | lint (model/effort pins, meta/throw)", run: cmdWorkflow, spec: workflowSpec},
		{name: "hook", hidden: true, summary: "run a lifecycle hook (invoked by settings.json)", run: agentcmd.Hook, spec: agentcmd.HookSpec},
		{name: "clip", hidden: true, summary: "save the Windows clipboard image as PNG; print its WSL path", run: clipboard.Clip, spec: clipboard.Spec},
		{name: "clip-clear", hidden: true, summary: "clear the clipboard after a delay if it still holds a copied secret", run: clipboard.ClipClear},
		{name: "wl-paste", hidden: true, summary: "WSL wl-paste proxy: answer agent image paste, else run real wl-paste", run: clipboard.WLPaste},
		{name: "version", hidden: true, summary: "print the build version", run: cmdVersion},
		{name: "completion", hidden: true, summary: "bash tab-completion script (--install to write it)", run: cmdCompletion, spec: completionSpec},
		{name: "__complete", hidden: true, summary: "machine: compute tab-completion candidates", run: cmdComplete},
	}
}

// completionBin is the installed binary name the completion script registers against.
const completionBin = "claude-code"

func completionEngine() helptext.Engine {
	return helptext.Engine{
		Tree: specTree,
		Resolve: func(word string) (string, bool) {
			if c := lookup(word); c != nil {
				return c.name, true
			}
			return "", false
		},
		Verbs: func() []helptext.Verb {
			var verbs []helptext.Verb
			for _, c := range commands() {
				verbs = append(verbs, helptext.Verb{Name: c.name, Hidden: c.hidden})
			}
			return verbs
		},
	}
}

func completionCandidates(words []string) []string {
	return completionEngine().Candidates(words)
}

// cmdComplete is the hidden completion verb behind the shell shim.
func cmdComplete(args []string) int {
	return completionEngine().Complete(args)
}

// completionPage: bare prints the script; zsh can source it after bashcompinit.
var completionPage = helptext.Page{
	Usage: "claude-code completion [bash] [flags]",
	About: "Print the bash tab-completion script (eval or source it). zsh: run " +
		"'autoload -U bashcompinit && bashcompinit', then source it.",
}

// parseCompletionArgs reports --install, or done with the exit code for -h and for an unknown word.
func parseCompletionArgs(args []string) (install bool, code int, done bool) {
	for _, a := range args {
		switch a {
		case "bash": // the only shell; accepted for the conventional `completion bash` spelling
		case "--install":
			install = true
		case "-h", "--help", "help":
			completionHelp()
			return false, 0, true
		default:
			fmt.Fprintf(os.Stderr, "completion: unknown argument %q (try -h)\n", a)
			return false, 2, true
		}
	}
	return install, 0, false
}

func completionHelp() {
	fs := helptext.NewFlagSet("completion", completionPage)
	fs.SetOutput(os.Stdout)
	fs.Bool("install", false, "install bash completion for new shells")
	fs.Usage()
}

// cmdCompletion prints the bash completion script (default) or, with --install, writes it to the
// bash-completion user dir (internal/helptext.Install).
func cmdCompletion(args []string) int {
	install, code, done := parseCompletionArgs(args)
	if done {
		return code
	}
	if !install {
		fmt.Print(helptext.Script(completionBin))
		return 0
	}
	path, err := helptext.Install(completionBin)
	if err != nil {
		return helptext.Fail("completion: %v", err)
	}
	fmt.Println(path)
	fmt.Println("installed — open a new shell (or 'source' the file) and double-Tab claude-code")
	return 0
}

// specTree returns the one completion spec tree, keyed by each verb's canonical name (command.name —
// see knownPath). Built once per process since tab completion is hot and commands() is fixed at startup.
func specTree() map[string]*helptext.Node {
	specTreeInit.Do(func() { specTreeCache = buildSpecTree() })
	return specTreeCache
}

var (
	specTreeInit  sync.Once
	specTreeCache map[string]*helptext.Node
)

// buildSpecTree gives every verb in commands() a node, an empty one when it has no spec, so a launcher is never unrepresented.
func buildSpecTree() map[string]*helptext.Node {
	tree := map[string]*helptext.Node{}
	for _, c := range commands() {
		tree[c.name] = &helptext.Node{}
		if c.spec != nil {
			tree[c.name] = c.spec()
		}
	}
	return tree
}

func completionSpec() *helptext.Node {
	return &helptext.Node{Flags: []helptext.Flag{helptext.BoolFlag("install")}}
}
