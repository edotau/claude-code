package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
)

// Main is the harness-hook entry: hook verbs run here, anything else (or a stamp mismatch) goes to claude-code.
func Main(args []string) int {
	isHook := len(args) >= 3 && args[1] == "hook"
	if full, ok := mainSibling(); ok && (!isHook || !stampMatches(filepath.Dir(full))) {
		// before AdoptEnvKey so claude-code keeps the vault key
		err := proc.Exec(full, args[1:], os.Environ())
		fmt.Fprintln(os.Stderr, "harness-hook: cannot run claude-code:", err)
		return 2
	}
	if !isHook {
		fmt.Fprintln(os.Stderr, "harness-hook: only hook verbs run here and claude-code is not beside it")
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
	full := filepath.Join(filepath.Dir(self), "claude-code")
	return full, paths.IsFile(full)
}

func stampMatches(dir string) bool {
	if paths.BuildStamp == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(dir, paths.StampFile))
	return err == nil && strings.TrimSpace(string(b)) == paths.BuildStamp
}
