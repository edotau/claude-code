// Package cli is the verb table, help, and command dispatch. Verb bodies live in per-area files.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/agent/agentcmd"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
)

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
