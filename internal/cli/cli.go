// Package cli is the verb table, help, and argv[0] shim dispatch. Verb bodies live in per-area files.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/statusline"
)

// command is one top-level verb; group places it on the menu, hidden keeps it dispatchable but off the menu.
type command struct {
	name    string
	summary string // ≤ summaryMax chars so a menu row never wraps at 80 columns
	group   string
	hidden  bool
	aliases []string // alternate spellings that dispatch here; never listed
	run     func(args []string) int
}

// Menu groups; declaration order in commands() is the display order.
const (
	groupAgents      = "AGENTS"
	groupAuth        = "AUTH"
	groupSession     = "SESSION"
	groupDiagnostics = "DIAGNOSTICS"
	groupSettings    = "SETTINGS"
)

// summaryMax caps a row summary so indent + nameWidth + summary stays inside 80 columns.
const (
	summaryMax = 60
	nameWidth  = 16
)

func commands() []command {
	return []command{
		// AGENTS — launch an agent CLI; `ask` runs one task headlessly.
		{name: "claude", group: groupAgents, summary: "launch Claude Code on the selected provider; args pass", run: cmdClaude},
		{name: "copilot", group: groupAgents, summary: "launch GitHub Copilot CLI on the registry; ask = one task", run: func(a []string) int { return vendorVerb("copilot", a) }},
		{name: "codex", group: groupAgents, summary: "launch Codex CLI on the registry; ask = one task", run: func(a []string) int { return vendorVerb("codex", a) }},
		{name: "gemini", group: groupAgents, summary: "launch the gemini CLI; ask|bridge = large-context", run: cmdGemini},
		{name: "opencode", group: groupAgents, summary: "launch OpenCode on the registry; ask = one task", run: func(a []string) int { return vendorVerb("opencode", a) }},
		// AUTH
		{name: "secrets", group: groupAuth, summary: "set|get|status, scan|git|all (audit), cred checks", run: cmdSecretsOrStub},
		{name: "resolve", group: groupAuth, aliases: []string{"token"}, summary: "resolve + print a provider's credential (apiKeyHelper)", run: cmdToken},
		{name: "profile", group: groupAuth, aliases: []string{"providers"}, summary: "providers + credential state; <name> shows its routes", run: cmdProviders},
		// SESSION
		{name: "switch", group: groupSession, aliases: []string{"use"}, summary: "pin the session provider (env.d/provider.env)", run: cmdUse},
		{name: "clean", group: groupSession, summary: "sweep accumulated state (dry-run; --apply deletes)", run: cmdClean},
		{name: "history", group: groupSession, summary: "shadow git history of config + memory: snapshot|log|diff", run: cmdHistory},
		{name: "science", group: groupSession, summary: "Claude for Life Sciences on its own OAuth (--print-env)", run: cmdScience},
		{name: "memory", group: groupSession, summary: "two-layer session memory: namespaced + shared global bank", run: cmdMemory},
		// DIAGNOSTICS
		{name: "doctor", group: groupDiagnostics, summary: "read-only harness health checks (see also mcp doctor)", run: cmdDoctor},
		{name: "generate", group: groupDiagnostics, aliases: []string{"settings"}, summary: "renders settings.json from the template (--out writes)", run: cmdSettings},
		{name: "usage", group: groupDiagnostics, summary: "subagent token totals + peak input tokens/min (--days N)", run: cmdUsage},
		{name: "mcp", group: groupDiagnostics, summary: "doctor: MCP server health, scope drift, inline secrets", run: cmdMCP},
		{name: "describe", group: groupDiagnostics, summary: "machine-readable map: verbs, providers, runners, hooks", run: cmdDescribe},
		// SETTINGS
		{name: "configure", group: groupSettings, summary: "guided setup: provider, login or key, models, agents", run: cmdConfigureOrStub},
		{name: "install", group: groupSettings, summary: "install Claude Code + write settings.json (onboarding)", run: cmdInstall},
		{name: "theme", group: groupSettings, summary: "pick the statusline color (--preview to list)", run: cmdTheme},
		{name: "vscode", group: groupSettings, summary: "merge the tracked VS Code settings template (--check)", run: cmdVSCode},
		// Hidden: dispatchable, named in the trailing note, off the menu.
		{name: "run", hidden: true, summary: "launch a vendor CLI (codex|gemini|opencode|copilot)", run: cmdRun},
		{name: "ask", hidden: true, summary: "one-shot task on an agent runner; --format json = envelope", run: cmdAsk},
		{name: "agents", hidden: true, summary: "list runners; sync --gemini | docs render the roster", run: cmdAgents},
		{name: "models", hidden: true, summary: "tier→model slots; --pin slot=[provider:]model, --live", run: cmdModels},
		{name: "router", hidden: true, summary: "loopback model router: serve | start | stop | status", run: cmdRouter},
		{name: "review", hidden: true, summary: "code-review detectors: quality|complexity|diff|pr|gate", run: cmdReview},
		{name: "workflow", hidden: true, summary: "skeleton <pattern> | lint (model/effort pins, meta/throw)", run: cmdWorkflow},
		{name: "hook", hidden: true, summary: "run a lifecycle hook (invoked by settings.json)", run: cmdHook},
		{name: "statusline", hidden: true, summary: "render the status line (invoked by settings.json)", run: cmdStatusline},
		{name: "version", hidden: true, summary: "print the build version", run: cmdVersion},
		{name: "completion", hidden: true, summary: "bash tab-completion script (--install to write it)", run: cmdCompletion},
		{name: "__complete", hidden: true, summary: "machine: compute tab-completion candidates", run: cmdComplete},
	}
}

// vendorVerb is the top-level spelling of a vendor agent: bare = launch (`run <v>`), `ask` = `ask --agent <v>`.
func vendorVerb(vendor string, args []string) int {
	if len(args) > 0 && args[0] == "ask" {
		return cmdAsk(append([]string{"--agent", vendor}, args[1:]...))
	}
	return launchVendor(vendor, args)
}

// shims maps a bare argv[0] to the verb it runs; `claude` is the launcher, vendors go through `run`.
var shims = map[string]func([]string) int{
	"claude":   cmdClaude,
	"gemini":   cmdGemini,
	"codex":    func(a []string) int { return launchVendor("codex", a) },
	"opencode": func(a []string) int { return launchVendor("opencode", a) },
	"copilot":  func(a []string) int { return launchVendor("copilot", a) },
}

// lookup finds a command by its name or one of its aliases.
func lookup(name string) *command {
	for _, c := range commands() {
		if c.name == name {
			return &c
		}
		for _, a := range c.aliases {
			if a == name {
				return &c
			}
		}
	}
	return nil
}

// Run dispatches argv and returns the exit code.
func Run(argv []string) int {
	if shim, ok := shims[strings.TrimSuffix(filepath.Base(argv[0]), ".exe")]; ok {
		return shim(argv[1:])
	}
	if len(argv) < 2 {
		return cmdClaude(nil)
	}
	name, args := argv[1], argv[2:]
	switch name {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return 0
	}
	if c := lookup(name); c != nil {
		return c.run(args)
	}
	// A leading flag with no verb is a claude flag: `claude-code --resume` == `claude-code claude --resume`.
	if strings.HasPrefix(name, "-") {
		return cmdClaude(argv[1:])
	}
	fmt.Fprintf(os.Stderr, "claude-code: unknown command %q (see `claude-code help`)\n", name)
	return 2
}

// usage renders the grouped menu: usage header, UPPERCASE groups, a gradient-painted name column, and a
// trailing note naming the hidden verbs and machine helpers. It opens on `usage:` — no banner.
func usage(w *os.File) {
	fmt.Fprintln(w, "usage: claude-code [<command>] [subcommand] [flags]")
	fmt.Fprintln(w, "       claude-code <command> -h    per-command help")
	cmds := commands()
	var groups []string
	seen := map[string]bool{}
	total := 0
	for _, c := range cmds {
		if c.hidden {
			continue
		}
		total++
		if !seen[c.group] {
			seen[c.group] = true
			groups = append(groups, c.group)
		}
	}
	paintHead := func(s string) string { return s }
	paintName := func(s string, frac float64) string { return s }
	if colorOK(w) {
		mode, stops, dim := statusline.Theme()
		paintHead = func(s string) string { return ansi.Paint(mode, dim, s, true) }
		paintName = func(s string, frac float64) string { return ansi.Paint(mode, ansi.GradAt(stops, frac), s, true) }
	}
	row := 0
	for _, g := range groups {
		fmt.Fprintln(w)
		fmt.Fprintln(w, paintHead(g))
		for _, c := range cmds {
			if c.hidden || c.group != g {
				continue
			}
			frac := float64(row) / float64(max(total-1, 1))
			row++
			fmt.Fprintf(w, "  %s%s\n", paintName(fmt.Sprintf("%-*s", nameWidth, c.name), frac), c.summary)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Drill into a command's verbs with `claude-code <command> -h` (e.g. memory -h).")
	fmt.Fprintln(w, "Hidden verbs: models, router, ask, agents, review, workflow, run.")
	fmt.Fprintln(w, "Machine helpers (hook, statusline) run from settings.json, not by hand.")
}

// colorOK is true when w is a terminal and NO_COLOR is unset (https://no-color.org).
func colorOK(w *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := w.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// sweep paints s rune by rune across the gradient, bold, like the status line's path segment.
func sweep(mode string, stops []ansi.RGB, s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		b.WriteString(ansi.FgSeq(mode, ansi.GradAt(stops, float64(i)/float64(max(len(runes)-1, 1))), true))
		b.WriteRune(r)
	}
	return b.String() + ansi.Reset
}

func cmdVersion([]string) int {
	v := "devel"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		v = bi.Main.Version
	}
	fmt.Println("claude-code", v)
	return 0
}

// fail prints a one-line error and returns exit code 1.
func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "claude-code: "+format+"\n", a...)
	return 1
}
