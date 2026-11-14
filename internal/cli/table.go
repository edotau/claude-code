// Package cli is the verb table, help, and command dispatch. Verb bodies live in per-area files.
package cli

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/doctor"
	"github.com/edotau/claude-code/internal/hosttest/ramdisk"
	"github.com/edotau/claude-code/internal/network"
	"github.com/edotau/claude-code/internal/network/egress"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/setup"
	"github.com/edotau/claude-code/internal/textutil"
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
	// sideEffectHelp marks a verb whose -h still acts (resolves credentials, runs a real binary).
	sideEffectHelp bool
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

var (
	commandsInit  sync.Once
	commandsCache []command
)

// commands returns the fixed verb table, built once per process; callers must not mutate it.
func commands() []command {
	commandsInit.Do(func() { commandsCache = buildCommands() })
	return commandsCache
}

func buildCommands() []command {
	return append([]command{
		// AGENTS — launch an agent CLI; `ask` runs one task headlessly.
		{name: "anthropic", group: groupAgents, summary: "Claude Code: --model opus|sonnet|haiku|fable; science", run: cmdAnthropic, spec: anthropicSpec},
		{name: "copilot", group: groupAgents, summary: "Copilot CLI on its own login, or your Databricks profile", run: func(a []string) int { return cmdAgentVerb("copilot", a) }, spec: agentSpec("copilot")},
		{name: "codex", group: groupAgents, summary: "Codex on your ChatGPT login; reviews", run: func(a []string) int { return cmdAgentVerb("codex", a) }, spec: agentSpec("codex")},
		{name: "gemini", group: groupAgents, summary: "Gemini CLI; 1M-context sweeps (ask --dirs)", run: func(a []string) int { return cmdAgentVerb("gemini", a) }, spec: agentSpec("gemini")},
		{name: "opencode", group: groupAgents, summary: "OpenCode on its own login, or your Databricks profile; docs", run: func(a []string) int { return cmdAgentVerb("opencode", a) }, spec: agentSpec("opencode")},
		{name: "databricks", group: groupAgents, summary: "Claude Code on Databricks; login, import/export, check --all", run: databricks.Run, spec: databricks.Spec},
		// AUTH
		{name: "resolve", group: groupAuth, hidden: true, summary: "resolve + print a profile's credential (apiKeyHelper)", run: cmdResolve, spec: resolveSpec},
		{name: "vault", group: groupAuth, summary: "pins, env, overlays and sealed secrets", run: cmdVault, spec: vaultSpec},
		{name: "network", group: groupAuth, summary: "tailnet, browsers, web search, remote agents", run: network.Run, spec: network.Spec},
		// SESSION
		{name: "switch", group: groupSession, hidden: true, summary: "set the session profile on every agent that serves it", run: cmdSwitch, spec: switchSpec},
		{name: "clean", group: groupSession, summary: "sweep accumulated state; --dry-run previews", run: func(args []string) int { return setup.Clean(args, os.Stdout, os.Stderr) }, spec: setup.CleanSpec},
		{name: "history", group: groupSession, summary: "shadow git history of the config files", run: cmdHistory, spec: historySpec},
		{name: "worktree", group: groupSession, summary: "manage worktrees in ~/worktrees/<repo>", run: cmdWorktree, spec: worktreeSpec},
		{name: "test", group: groupSession, hidden: true, summary: "test tooling: run a command with TMPDIR on a RAM disk", run: cmdTest, spec: testSpec},
		{name: "memory", group: groupSession, summary: "session memory bank: recall, save, promote", run: cmdMemory, spec: memorySpec},
		{name: "plan", group: groupSession, hidden: true, summary: "plan checklists: status (ticked/total, claims), tick", run: cmdPlan, spec: planSpec},
		{name: "scm", group: groupSession, summary: "PRs, checks, comments on GitHub or Bitbucket DC", run: cmdScm, spec: scmSpec},
		// DIAGNOSTICS
		{name: "doctor", group: groupDiagnostics, summary: "read-only health checks (--audit: action log)", run: doctor.Run, spec: doctor.SpecNode},
		{name: "gateway", group: groupDiagnostics, summary: "which agent + model a task routes to; the loopback proxy", run: cmdGateway, spec: gatewaySpec},
		{name: "usage", group: groupDiagnostics, summary: "token totals, peak rate, profile quotas, context by source", run: func(a []string) int { return usagecmd.Run(a, paths.ConfigDir(), os.Stdout, os.Stderr) }, spec: usagecmd.Spec},
		// SETTINGS
		{name: "install", group: groupSettings, summary: "install claude-code and agent links; seed settings.json", run: setup.RunInstall, spec: setup.InstallSpec},
		{name: "configure", group: groupSettings, summary: "guided setup; --restore resets settings.json", run: cmdConfigure, spec: configureSpec},
		{name: "theme", group: groupSettings, summary: "palette, statusline, VS Code template, tool themes", run: cmdTheme, spec: themeSpec},
	}, hiddenCommands()...)
}

func hiddenCommands() []command {
	return []command{
		// Hidden: dispatchable, off the menu (documented in README.md and rules/).
		{name: "claude", hidden: true, summary: "launch Claude Code on the selected profile", run: cmdClaude, sideEffectHelp: true},
		{name: "help-svg", hidden: true, summary: "render this menu as the README SVG (--out f)", run: cmdMenuSVG, spec: menuSVGSpec},
		{name: "agents", hidden: true, summary: "ask, dispatch, list, sync, tree, status: run and track agents", run: cmdAgents, spec: agentsSpec},
		{name: "preflight", hidden: true, summary: "pin check per agent; --live runs the post-install pong round", run: cmdPreflight, spec: preflightSpec},
		{name: "models", hidden: true, summary: "model aliases; --pin alias=model, --live", run: cmdModels, spec: modelsSpec},
		{name: "review", hidden: true, summary: "code-review detectors: quality, complexity, simplify, diff, pr, gate, ...", run: cmdReview, spec: reviewSpec},
		{name: "workflow", hidden: true, summary: "skeleton <pattern> | lint (pins, meta/throw, syntax, table)", run: cmdWorkflow, spec: workflowSpec},
		{name: "hook", hidden: true, summary: "run a lifecycle hook (invoked by settings.json)", run: cmdHook, spec: hookSpec},
		{name: "clip", hidden: true, summary: "save the clipboard image as PNG and print its path", run: cmdClip, spec: clipSpec},
		{name: "clip-clear", hidden: true, summary: "clear the clipboard after a delay if it still holds a copied secret", run: cmdClipClear, spec: clipClearSpec},
		{name: "wl-paste", hidden: true, summary: "WSL wl-paste proxy: answer agent image paste, else run real wl-paste", run: cmdWLPaste, sideEffectHelp: true},
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

// cmdComplete is the hidden completion verb behind the shell shim.
func cmdComplete(args []string) int {
	for _, c := range remote.Complete(args, egressFlag, completionEngine().Candidates) {
		fmt.Println(c)
	}
	return 0
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

// cmdCompletion prints the bash completion script, or with --install writes it to the user dir.
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

// specTree is the completion spec tree keyed by verb name (see knownPath), built once per process.
func specTree() map[string]*helptext.Node {
	specTreeInit.Do(func() { specTreeCache = buildSpecTree() })
	return specTreeCache
}

var (
	specTreeInit  sync.Once
	specTreeCache map[string]*helptext.Node
)

// buildSpecTree gives every verb in commands() a node, an empty one when it has no spec.
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

// isHelpVerb reports -h, --help or help.
func isHelpVerb(arg string) bool { return arg == "-h" || arg == "--help" || arg == "help" }

// unknownSub reports an unknown subcommand of area, prints its page to stderr and returns usage exit 2.
func unknownSub(area, sub string, page helptext.Page) int {
	helptext.Fail("%s: unknown subcommand %q", area, sub)
	page.Fprint(os.Stderr)
	return 2
}

// helpOrBare prints page for a bare verb (stderr) or an explicit -h (stdout); ok is false when args ask for neither.
func helpOrBare(page helptext.Page, args []string) (code int, ok bool) {
	switch {
	case len(args) == 0:
		page.Fprint(os.Stderr)
	case isHelpVerb(args[0]):
		page.Fprint(os.Stdout)
	default:
		return 0, false
	}
	return 0, true
}

// lookup finds a command by its name.
func lookup(name string) *command {
	for _, c := range commands() {
		if c.name == name {
			return &c
		}
	}
	return nil
}

// Run dispatches argv and returns the exit code.
func Run(argv []string) int {
	paths.ExportConfigDir()
	egress.Install()
	if len(argv) < 2 {
		return printUsage()
	}
	name, args := argv[1], argv[2:]
	if isHelpVerb(name) {
		return printUsage()
	}
	if code, ok := runRouted(argv, name); ok {
		return code
	}
	if c := lookup(name); c != nil {
		return c.run(args)
	}
	// A leading flag with no verb is a claude flag: `claude-code --resume` launches Claude Code.
	if strings.HasPrefix(name, "-") {
		return cmdClaude(argv[1:])
	}
	// A profile name no verb shadows launches Claude Code on it: `claude-code ollama`.
	if reg, err := providers.Load(); err == nil {
		if _, ok := reg.Providers[name]; ok {
			return cmdClaude(append([]string{"--profile", name}, args...))
		}
	}
	fmt.Fprintf(os.Stderr, "claude-code: unknown command %q (see `claude-code help`)\n", name)
	return 2
}

// runRouted handles --host (verb over ssh on a tailnet host) and --remote (egress via an ssh host).
func runRouted(argv []string, name string) (code int, ok bool) {
	if err := remote.CheckLogin(argv[1:], egressFlag); err != nil {
		return helptext.FailUsage("%v", err), true
	}
	if host, auto := remote.AutoLogin(argv[1:]); auto { // CLAUDE_REMOTE_HOST set: a plain login is `--remote <host>`
		if err := proc.Setenv(remote.LoginAutoEnv, host); err != nil {
			return helptext.Fail("%s: %v", egressFlag, err), true
		}
		return runVia(append([]string{argv[0], egressFlag, host}, argv[1:]...)), true
	}
	if name == remote.Flag || strings.HasPrefix(name, remote.Flag+"=") {
		if rest, local := remote.LocalArgs(remote.Flag, argv[1:]); local { // the named host is this machine: run here
			return Run(append(argv[:1:1], rest...)), true
		}
		return remote.Run(argv[1:]), true
	}
	if name == egressFlag || strings.HasPrefix(name, egressFlag+"=") {
		if rest, local := remote.LocalArgs(egressFlag, argv[1:]); local { // this machine is the exit: no tunnel
			return Run(append(argv[:1:1], rest...)), true
		}
		return runVia(argv), true
	}
	return 0, false
}

// egressFlag runs a verb here with its Databricks workspace requests leaving through an ssh host.
const egressFlag = "--remote"

// runVia is `claude-code --remote <host> <verb …>`: it pins the egress host for this process and its children.
func runVia(argv []string) int {
	host, rest, err := remote.SplitHost(egressFlag, argv[1:])
	if err != nil {
		return helptext.FailUsage("%v", err)
	}
	if host, err = remote.ResolveAuto(host); err != nil { // `--remote auto`: the other machine, as `--host auto`
		return helptext.Fail("%s %v", egressFlag, err)
	}
	if err := egress.Valid(host); err != nil {
		return helptext.FailUsage("%s: %v", egressFlag, err)
	}
	if err := proc.Setenv(egress.EnvHost, host); err != nil {
		return helptext.Fail("%s: %v", egressFlag, err)
	}
	if err := remote.PinLogin(host, rest); err != nil {
		return helptext.Fail("%s: %v", egressFlag, err)
	}
	return Run(append(argv[:1:1], rest...))
}

// printUsage writes the menu to stdout; it returns 1 when the write fails.
func printUsage() int {
	if err := usage(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: write help: %v\n", err)
		return 1
	}
	return 0
}

var versionPage = helptext.Page{Usage: "claude-code version", About: "Print the build version."}

func cmdVersion(args []string) int {
	if len(args) > 0 && isHelpVerb(args[0]) {
		versionPage.Fprint(os.Stderr)
		return 0
	}
	fmt.Println("claude-code", textutil.BuildVersion())
	return 0
}

// cmdTest dispatches `test <sub>`: test tooling.
func cmdTest(args []string) int {
	if len(args) > 0 && args[0] == "ramdisk" {
		return ramdisk.Run(args[1:])
	}
	if code, ok := helpOrBare(testPage, args); ok {
		return code
	}
	return unknownSub("test", args[0], testPage)
}

var testPage = helptext.Page{
	Usage:    "claude-code test <command>",
	About:    "Test tooling.",
	Commands: [][2]string{{"ramdisk", "run a command with TMPDIR on a throwaway RAM disk"}},
}

// testSpec is the completion node for the test verb tree.
func testSpec() *helptext.Node { return &helptext.Node{Sub: map[string]*helptext.Node{"ramdisk": {}}} }
