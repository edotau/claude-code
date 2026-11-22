package hooks

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// Main is the bin/.hooks entry: every call runs claude-code beside it; alone, only hook verbs run here.
func Main(args []string) int {
	isHook := len(args) >= 3 && args[1] == "hook"
	// Hand every call over: modules read registries (agent roster, ask flags) that only claude-code links today.
	if full, ok := mainSibling(); ok {
		// before AdoptEnvKey so claude-code keeps the vault key
		err := proc.Exec(full, args[1:], os.Environ())
		fmt.Fprintln(os.Stderr, ".hooks: cannot run claude-code:", err)
		return 2
	}
	if !isHook {
		fmt.Fprintln(os.Stderr, ".hooks: only hook verbs run here and claude-code is not beside it")
		return 2
	}
	vault.AdoptEnvKey()
	code := RunArgs(args[2], args[3:], os.Stdin, os.Stdout, os.Stderr)
	providers.FinishExchanges()
	return code
}

func mainSibling() (string, bool) {
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	if real, err := filepath.EvalSymlinks(self); err == nil { // a symlink elsewhere still finds its own siblings
		self = real
	}
	full := filepath.Join(filepath.Dir(self), "claude-code")
	return full, paths.IsFile(full)
}
