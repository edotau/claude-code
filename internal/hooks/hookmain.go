package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
)

// Main is the bin/.hookd entry: `<Event> …` (or `hook <Event> …`) runs here; anything else goes to claude-code.
func Main(args []string) int {
	rest := args[1:]
	if len(rest) >= 2 && rest[0] == "hook" {
		rest = rest[1:] // the older `.hookd hook <Event>` form
	}
	isHook := len(rest) >= 1 && slices.Contains(settings.HookEvents(), rest[0])
	if full, ok := mainSibling(); ok && (!isHook || !stampMatches(filepath.Dir(full))) {
		fwd := args[1:]
		if isHook {
			fwd = append([]string{"hook"}, rest...)
		}
		// before AdoptEnvKey so claude-code keeps the vault key
		err := proc.Exec(full, fwd, os.Environ())
		fmt.Fprintln(os.Stderr, ".hookd: cannot run claude-code:", err)
		if !isHook {
			return 2
		}
	}
	if !isHook {
		fmt.Fprintln(os.Stderr, ".hookd: only hook verbs run here and claude-code is not beside it")
		return 2
	}
	vault.AdoptEnvKey()
	code := RunArgs(rest[0], rest[1:], os.Stdin, os.Stdout, os.Stderr)
	providers.FinishExchanges()
	return code
}

var mainSibling = findMainSibling

func findMainSibling() (string, bool) {
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

func stampMatches(dir string) bool {
	if paths.BuildStamp == "" {
		return false
	}
	stamp := filepath.Join(dir, paths.StampFile)
	b, err := os.ReadFile(stamp)
	if err != nil {
		return false
	}
	main, err := os.Stat(filepath.Join(dir, "claude-code"))
	if err != nil || !main.Mode().IsRegular() {
		return false
	}
	want := fmt.Sprintf("%s %d %d", paths.BuildStamp, main.Size(), main.ModTime().Unix())
	return strings.TrimSpace(string(b)) == want
}
