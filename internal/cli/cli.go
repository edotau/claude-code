// Package cli is the verb table, help, and command dispatch. Verb bodies live in per-area files.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/edotau/claude-code/internal/network/remote"

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
		return printUsage()
	}
	name, args := argv[1], argv[2:]
	switch name {
	case "-h", "--help", "help":
		return printUsage()
	}
	if name == "--host" || strings.HasPrefix(name, "--host=") { // run a harness verb on a tailnet host over ssh
		return remote.Run(argv[1:])
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

// printUsage writes the menu to stdout; it returns 1 when the write fails.
func printUsage() int {
	if err := usage(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: write help: %v\n", err)
		return 1
	}
	return 0
}

func cmdVersion([]string) int {
	fmt.Println("claude-code", textutil.BuildVersion())
	return 0
}
