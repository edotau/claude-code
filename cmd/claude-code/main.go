// Command claude-code is the standalone Claude Code harness: launcher, model router, agent runners, hooks.
package main

import (
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/cli"
	_ "github.com/edotau/claude-code/internal/memory/shadow"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	_ "github.com/edotau/claude-code/internal/proxy/phase"
	"github.com/edotau/claude-code/internal/vault"
)

func main() {
	vault.AdoptEnvKey() // before any child can inherit the master key; hooks keep it in memory, not in env
	stop := func() {}
	if len(os.Args) < 2 || os.Args[1] != "hook" { // short-lived hooks stay untouched
		stop = proc.ProfileOnSignal(func() string { return filepath.Join(paths.ConfigDir(), "state", "profiles") })
	}
	code := cli.Run(os.Args)
	stop()
	// a refresh token already sent must reach the vault before the process exits
	providers.FinishExchanges()
	os.Exit(code)
}
