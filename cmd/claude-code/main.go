// Command claude-code is the standalone Claude Code harness: launcher, model router, agent runners, hooks.
package main

import (
	"os"

	"github.com/edotau/claude-code/internal/cli"
	"github.com/edotau/claude-code/internal/vault"
)

func main() {
	vault.AdoptEnvKey() // before any child can inherit the master key; hooks keep it in memory, not in env
	os.Exit(cli.Run(os.Args))
}
