// Package cli is the verb table, help, and command dispatch. Verb bodies live in per-area files.
package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent/agentcmd"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// agentVerb is the top-level spelling of an agent: bare = launch, `--` = forced passthrough,
// `ask` = `ask --agent <v>`, a word in agentSubverbs = agentSub, else launch with args forwarded.
func agentVerb(name string, args []string) int {
	if len(args) == 0 {
		return agentcmd.LaunchAgent(name, args)
	}
	if args[0] == "--" {
		return agentcmd.LaunchAgent(name, args[1:])
	}
	if args[0] == "ask" {
		return ask.Cmd(append([]string{"--agent", name}, args[1:]...))
	}
	if agentSubverbs[args[0]] {
		return agentSub(name, args)
	}
	if len(args) == 1 && slices.Contains([]string{"-h", "--help", "help"}, args[0]) {
		agentPage(name).Fprint(os.Stderr)
		return 0
	}
	if len(args) == 1 && args[0] == helptext.DocsFlag {
		s, _ := agent.LookupSchema(name)
		return helptext.WrappedDocs(s.CLI.Binary)
	}
	return agentcmd.LaunchAgent(name, args)
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
		return agentcmd.Claude(argv[1:])
	}
	// A profile name no verb shadows launches Claude Code on it: `claude-code ollama`.
	if reg, err := providers.Load(); err == nil {
		if _, ok := reg.Providers[name]; ok {
			return agentcmd.Claude(append([]string{"--profile", name}, args...))
		}
	}
	fmt.Fprintf(os.Stderr, "claude-code: unknown command %q (see `claude-code help`)\n", name)
	return 2
}

func cmdVersion([]string) int {
	fmt.Println("claude-code", textutil.BuildVersion())
	return 0
}
