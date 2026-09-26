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

type command struct {
	name    string
	summary string
	run     func(args []string) int
}

func commands() []command {
	return []command{
		{"claude", "launch Claude Code on the selected provider (default with no command, or when invoked as `claude`)", cmdClaude},
		{"run", "launch a vendor agent CLI (codex|gemini|opencode|copilot) routed through the registry", cmdRun},
		{"providers", "list providers, or show one's routes and credential state", cmdProviders},
		{"models", "show resolved tier→model slots; --pin slot=[provider:]model, --unpin, --live", cmdModels},
		{"use", "pin the session provider (env.d/provider.env)", cmdUse},
		{"token", "print the credential for a provider (Claude Code apiKeyHelper)", cmdToken},
		{"router", "loopback model router: serve | start | stop | status", cmdRouter},
		{"ask", "one-shot task on an agent runner; --format json prints the envelope", cmdAsk},
		{"agents", "list agent runners; sync --gemini | docs mirror the roster into ~/.gemini/skills and AGENTS.md/GEMINI.md", cmdAgents},
		{"gemini", "gemini agent: ask <task> | bridge [--dirs|--files] <task> (one 1M-context call) | <vendor args>", cmdGemini},
		{"review", "code-review detectors: quality | complexity | assumptions | goals | diff | pr | report | gate", cmdReview},
		{"workflow", "workflow skeleton <pattern> [--name n]: scaffold a Workflow tool script", cmdWorkflow},
		{"science", "launch Claude for Life Sciences (claude-science) without the harness env; --print-env shows it", cmdScience},
		{"memory", "session memory bank: init | search | index | path | update", cmdMemory},
		{"hook", "run a lifecycle hook (invoked by settings.json)", cmdHook},
		{"statusline", "render the status line (invoked by settings.json)", cmdStatusline},
		{"settings", "render settings.json from the template (--out to write, backing up)", cmdSettings},
		{"install", "build shims into ~/.claude/bin and render settings.json", cmdInstall},
		{"doctor", "check binaries, providers, credentials, and router health", cmdDoctor},
		{"version", "print the build version", cmdVersion},
	}
}

// shims maps a bare argv[0] to the verb it runs; `claude` is the launcher, vendors go through `run`.
var shims = map[string]func([]string) int{
	"claude":   cmdClaude,
	"gemini":   cmdGemini,
	"codex":    func(a []string) int { return launchVendor("codex", a) },
	"opencode": func(a []string) int { return launchVendor("opencode", a) },
	"copilot":  func(a []string) int { return launchVendor("copilot", a) },
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
	for _, c := range commands() {
		if c.name == name {
			return c.run(args)
		}
	}
	// A leading flag with no verb is a claude flag: `claude-code --resume` == `claude-code claude --resume`.
	if strings.HasPrefix(name, "-") {
		return cmdClaude(argv[1:])
	}
	fmt.Fprintf(os.Stderr, "claude-code: unknown command %q (see `claude-code help`)\n", name)
	return 2
}

func usage(w *os.File) {
	title, tagline, section := "claude-code", " — standalone Claude Code harness", func(s string) string { return s }
	cmds := commands()
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = fmt.Sprintf("%-11s", c.name)
	}
	if colorOK(w) {
		mode, stops, dim := statusline.Theme()
		title = sweep(mode, stops, title)
		tagline = ansi.Paint(mode, dim, tagline, false)
		section = func(s string) string { return ansi.Paint(mode, dim, s, true) }
		for i := range names {
			names[i] = ansi.Paint(mode, ansi.GradAt(stops, float64(i)/float64(max(len(names)-1, 1))), names[i], true)
		}
	}
	fmt.Fprintf(w, "%s%s\n\n%s claude-code <command> [args]\n\n%s\n", title, tagline, section("Usage:"), section("Commands:"))
	for i, c := range cmds {
		fmt.Fprintf(w, "  %s %s\n", names[i], c.summary)
	}
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
