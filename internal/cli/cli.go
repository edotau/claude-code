// Package cli is the verb table, help, and argv[0] shim dispatch. Verb bodies live in per-area files.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
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
		{"agents", "list agent runners and whether each is available", cmdAgents},
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
	"codex":    func(a []string) int { return launchVendor("codex", a) },
	"gemini":   func(a []string) int { return launchVendor("gemini", a) },
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
	fmt.Fprintln(w, "claude-code — standalone Claude Code harness\n\nUsage: claude-code <command> [args]\n\nCommands:")
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-11s %s\n", c.name, c.summary)
	}
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
