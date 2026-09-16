// Command claude-code is the standalone Claude Code harness: launcher, model router, agent runners, hooks.
package main

import (
	"os"

	"github.com/edotau/claude-code/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args)) }
