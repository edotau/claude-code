package safety

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
)

// symlinkMaker spots a command that could point a temp path into a real directory before the removal runs.
var symlinkMaker = regexp.MustCompile(`(^|[^[:alnum:]_])ln\s|symlink`)

// darwinUserTemp is the per-user temp folder on macOS (what TMPDIR names), in its resolved /private form.
var darwinUserTemp = regexp.MustCompile(`^/(private/)?var/folders/[^/]+/[^/]+/T(/|$)`)

// removalAllowed reports whether every path cmd removes is a literal temp path outside the live config dir and home;
// truncate follows a final symlink and ln can aim a temp path at the real tree, so either one voids the allowance.
func removalAllowed(cmd, cwd string) bool {
	if strings.Contains(cmd, "truncate") || symlinkMaker.MatchString(cmd) {
		return false
	}
	return !slices.ContainsFunc(core.RemovedPaths(cmd, cwd), func(p string) bool { return !tempRemovable(p) })
}

// tempRemovable reports whether deleting p (the final element, never what it points at) touches only a temp folder.
func tempRemovable(p string) bool {
	if !filepath.IsAbs(p) || strings.ContainsAny(p, "*?[]{}$`") || slices.Contains(strings.Split(p, "/"), "..") {
		return false
	}
	target := filepath.Join(paths.CanonDeep(filepath.Dir(p)), filepath.Base(p))
	for _, live := range liveRoots() {
		if paths.Within(foldCase(target), foldCase(live)) {
			return false
		}
	}
	return underTemp(target)
}

// liveRoots are the trees a removal must never reach even when they sit under a temp root: the config dir, ~/.claude
// and the home directory, each symlink-resolved.
func liveRoots() []string {
	roots := []string{core.CanonPath(paths.ConfigDir())}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, core.CanonPath(filepath.Join(home, ".claude")), core.CanonPath(home))
	}
	return roots
}

// underTemp reports whether the resolved target sits under os.TempDir(), /tmp, /private/tmp or the macOS user temp.
func underTemp(target string) bool {
	if runtime.GOOS == "darwin" && darwinUserTemp.MatchString(target) {
		return true
	}
	for _, root := range []string{os.TempDir(), "/tmp", "/private/tmp"} {
		for _, r := range []string{filepath.Clean(root), core.CanonPath(root)} {
			if r != "/" && paths.Within(target, r) {
				return true
			}
		}
	}
	return false
}
