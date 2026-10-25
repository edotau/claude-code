// Package cli is the verb table, help, and command dispatch. Verb bodies live in per-area files.
package cli

import (
	"fmt"
	"os"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/statusline"
)

// command is one top-level verb; group places it on the menu, hidden keeps it dispatchable but off the menu.
type command struct {
	name    string
	summary string // aim for ≤ summaryMax chars; longer wraps with a hanging indent
	group   string
	hidden  bool
	aliases []string // alternate spellings that dispatch here; never listed
	run     func(args []string) int
}

// Menu groups; declaration order in commands() is the display order.
const (
	groupAnthropic   = "ANTHROPIC"
	groupVendors     = "AGENTS"
	groupAuth        = "AUTH"
	groupSession     = "SESSION"
	groupDiagnostics = "DIAGNOSTICS"
	groupSettings    = "SETTINGS"
)

// alphabetical groups list their verbs A→Z; ANTHROPIC (tier order) and AGENTS keep declaration order.
var alphabetical = map[string]bool{groupAuth: true, groupSession: true, groupDiagnostics: true, groupSettings: true}

// summaryMax is a soft target: longer summaries wrap with a hanging indent (helptext.WrapRow); one line is preferred.
const (
	menuWidth  = 80
	summaryMax = 60
	nameWidth  = 16
)

func commands() []command {
	return []command{
		// AGENTS — launch an agent CLI; `ask` runs one task headlessly.
		{name: "opus", group: groupAnthropic, summary: "high reasoning: deep architecture · $$$", run: tierVerb(models.Opus)},
		{name: "sonnet", group: groupAnthropic, summary: "everyday coding, reviews, subagents · $$", run: tierVerb(models.Sonnet)},
		{name: "haiku", group: groupAnthropic, summary: "quick lookups, mechanical edits · $ fastest", run: tierVerb(models.Haiku)},
		{name: "fable", group: groupAnthropic, summary: "separate frontier family; main-session default · $$$$", run: tierVerb(models.Fable)},
		{name: "science", group: groupAnthropic, summary: "Claude for Life Sciences, own login (--print-env)", run: cmdScience},
		{name: "copilot", group: groupVendors, summary: "Copilot CLI on your Copilot plan", run: func(a []string) int { return vendorVerb("copilot", a) }},
		{name: "codex", group: groupVendors, summary: "Codex on your ChatGPT login; reviews", run: func(a []string) int { return vendorVerb("codex", a) }},
		{name: "gemini", group: groupVendors, summary: "Gemini CLI; 1M-context sweeps (bridge)", run: cmdGemini},
		{name: "opencode", group: groupVendors, summary: "OpenCode on your opencode Go plan; docs and drafting", run: func(a []string) int { return vendorVerb("opencode", a) }},
		{name: "databricks", group: groupVendors, summary: "Claude Code on Databricks serving; check: smoke/1M probe", run: databricks.Run},
		// AUTH
		{name: "resolve", group: groupAuth, hidden: true, aliases: []string{"token"}, summary: "resolve + print a provider's credential (apiKeyHelper)", run: cmdToken},
		{name: "vault", group: groupAuth, summary: "pins, env, overlays and sealed secrets", run: cmdVault},
		{name: "network", group: groupAuth, summary: "tailnet, browsers, web search, MCP health, agent page", run: cmdNetwork},
		// SESSION
		{name: "switch", group: groupSession, hidden: true, aliases: []string{"use"}, summary: "pin the session provider (vault pins)", run: cmdUse},
		{name: "clean", group: groupSession, summary: "sweep accumulated state; --dry-run previews", run: cmdClean},
		{name: "history", group: groupSession, summary: "shadow git history of config + memory", run: cmdHistory},
		{name: "dispatch", group: groupSession, summary: "run an agent on a tailnet host in tmux", run: cmdDispatch},
		{name: "profile", group: groupSession, summary: "per-agent account profiles: list, use, save, migrate", run: cmdProfile},
		{name: "worktree", group: groupSession, summary: "manage worktrees in ~/worktrees/<repo>", run: cmdWorktree},
		{name: "memory", group: groupSession, summary: "session memory bank: recall, save, promote", run: cmdMemory},
		// DIAGNOSTICS
		{name: "doctor", group: groupDiagnostics, summary: "read-only health checks (--audit: action log)", run: cmdDoctor},
		{name: "route", group: groupDiagnostics, summary: "which runner + model a tier routes to, and why", run: cmdRoute},
		{name: "usage", group: groupDiagnostics, summary: "token totals, peak rate, provider quotas", run: cmdUsage},
		{name: "describe", group: groupDiagnostics, summary: "machine-readable map of the harness", run: cmdDescribe},
		// SETTINGS
		{name: "install", group: groupSettings, summary: "install Claude Code + write settings.json", run: cmdInstall},
		{name: "configure", group: groupSettings, summary: "guided setup; --reset re-renders from the templates", run: cmdConfigure},
		{name: "theme", group: groupSettings, summary: "palette, statusline, VS Code template, tool themes", run: cmdTheme},
		// Hidden: dispatchable, named in the trailing note, off the menu.
		{name: "claude", hidden: true, summary: "launch Claude Code on the selected provider", run: cmdClaude},
		{name: "help-svg", hidden: true, summary: "render this menu as the README SVG (--out f, --check f)", run: cmdMenuSVG},
		{name: "ask", hidden: true, summary: "one-shot task on an agent runner; --format json = envelope", run: cmdAsk},
		{name: "agents", hidden: true, summary: "list runners; sync roster + instructions into vendor homes", run: cmdAgents},
		{name: "models", hidden: true, summary: "tier→model slots; --pin slot=[provider:]model, --live", run: cmdModels},
		{name: "router", hidden: true, summary: "loopback model router: serve | start | stop | status", run: cmdRouter},
		{name: "review", hidden: true, summary: "code-review detectors: quality|complexity|diff|pr|gate", run: cmdReview},
		{name: "workflow", hidden: true, summary: "skeleton <pattern> | lint (model/effort pins, meta/throw)", run: cmdWorkflow},
		{name: "hook", hidden: true, summary: "run a lifecycle hook (invoked by settings.json)", run: cmdHook},
		{name: "clip", hidden: true, summary: "save the Windows clipboard image as PNG; print its WSL path", run: cmdClip},
		{name: "bmp-transcode", hidden: true, summary: "stdin BMP → stdout 24bpp BMP (--png: PNG); wl-paste proxy", run: cmdBMPTranscode},
		{name: "version", hidden: true, summary: "print the build version", run: cmdVersion},
		{name: "completion", hidden: true, summary: "bash tab-completion script (--install to write it)", run: cmdCompletion},
		{name: "__complete", hidden: true, summary: "machine: compute tab-completion candidates", run: cmdComplete},
	}
}

// vendorVerb is the top-level spelling of a vendor agent: bare = launch, `--` = forced passthrough,
// `ask` = `ask --agent <v>`, a word in vendorSubverbs = cmdVendorSub, else launch with args forwarded.
func vendorVerb(vendor string, args []string) int {
	if len(args) > 0 && args[0] == "--" {
		return launchVendor(vendor, args[1:])
	}
	if len(args) > 0 && args[0] == "ask" {
		return cmdAsk(append([]string{"--agent", vendor}, args[1:]...))
	}
	if len(args) > 0 && vendorSubverbs[args[0]] {
		return cmdVendorSub(vendor, args)
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		vendorPage(vendor).Fprint(os.Stderr)
		return 0
	}
	return launchVendor(vendor, args)
}

// lookup finds a command by its name or one of its aliases.
func lookup(name string) *command {
	for _, c := range commands() {
		if c.name == name {
			return &c
		}
		if slices.Contains(c.aliases, name) {
			return &c
		}
	}
	return nil
}

// Run dispatches argv and returns the exit code.
func Run(argv []string) int {
	if len(argv) < 2 {
		usage(os.Stdout)
		return 0
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
	// A leading flag with no verb is a claude flag: `claude-code --resume` launches Claude Code.
	if strings.HasPrefix(name, "-") {
		return cmdClaude(argv[1:])
	}
	// A provider name no verb shadows launches Claude Code on it: `claude-code ollama`.
	if reg, err := providers.Load(); err == nil {
		if _, ok := reg.Providers[name]; ok {
			return cmdClaude(append([]string{"--provider", name}, args...))
		}
	}
	fmt.Fprintf(os.Stderr, "claude-code: unknown command %q (see `claude-code help`)\n", name)
	return 2
}

// menuHeader is the menu's fixed opening; it opens on `usage:` — no banner.
var menuHeader = []string{
	"usage: claude-code [<command>] [subcommand] [flags]",
	"       claude-code <command> -h    per-command help",
}

// menuRow is one line after the header: blank, a group head, or a verb (name + summary at gradient position frac).
type menuRow struct {
	head, name, summary string
	frac                float64
}

// menuRows lays out the visible verbs: UPPERCASE groups in declaration order, each preceded by a blank line.
func menuRows() []menuRow {
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
	var rows []menuRow
	n := 0
	for _, g := range groups {
		rows = append(rows, menuRow{}, menuRow{head: g})
		var group []command
		for _, c := range cmds {
			if !c.hidden && c.group == g {
				group = append(group, c)
			}
		}
		if alphabetical[g] {
			slices.SortStableFunc(group, func(a, b command) int { return strings.Compare(a.name, b.name) })
		}
		for _, c := range group {
			rows = append(rows, menuRow{name: fmt.Sprintf("%-*s", nameWidth, c.name), summary: c.summary,
				frac: float64(n) / float64(max(total-1, 1))})
			n++
		}
	}
	return rows
}

// usage renders the grouped menu, painting heads dim and the name column along the status line's gradient.
func usage(w *os.File) {
	for _, l := range menuHeader {
		fmt.Fprintln(w, l)
	}
	paintHead := func(s string) string { return s }
	paintName := func(s string, frac float64) string { return s }
	if colorOK(w) {
		mode, stops, dim := statusline.Theme()
		paintHead = func(s string) string { return ansi.Paint(mode, dim, s, true) }
		paintName = func(s string, frac float64) string { return ansi.Paint(mode, ansi.GradAt(stops, frac), s, true) }
	}
	for _, r := range menuRows() {
		switch {
		case r.head != "":
			fmt.Fprintln(w, paintHead(r.head))
		case r.name != "":
			lines := helptext.WrapRow("  "+r.name, r.summary, 2+nameWidth, menuWidth)
			fmt.Fprintf(w, "  %s%s\n", paintName(r.name, r.frac), lines[0][2+nameWidth:])
			for _, l := range lines[1:] {
				fmt.Fprintln(w, l)
			}
		default:
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintln(w) // Why: a blank line between the menu and the next shell prompt
}

// colorOK is true when w is a terminal and NO_COLOR is unset (https://no-color.org).
func colorOK(w *os.File) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	return ansi.IsTerminal(w)
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
