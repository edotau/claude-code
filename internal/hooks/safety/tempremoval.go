package safety

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/shell"
)

// symlinkMaker spots a command that could point a temp path into a real directory before the removal runs.
var symlinkMaker = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(^|[^[:alnum:]_])ln\s|symlink`) })

// darwinUserTemp is the per-user temp folder on macOS (what TMPDIR names), in its resolved /private form.
var darwinUserTemp = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^/(private/)?var/folders/[^/]+/[^/]+/T(/|$)`)
})

// removalAllowed reports whether every path cmd removes is a literal temp path outside the live config dir and home;
// truncate follows a final symlink and ln can aim a temp path at the real tree, so either one voids the allowance.
func removalAllowed(cmd string, removed []string) bool {
	if strings.Contains(cmd, "truncate") || symlinkMaker().MatchString(cmd) {
		return false
	}
	return !slices.ContainsFunc(removed, func(p string) bool { return !tempRemovable(p) })
}

// movesCwd spots a cd, pushd/popd or chdir option, after which a relative path may no longer mean what it says.
var movesCwd = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(^|[^[:alnum:]_.-])(cd|pushd|popd)(\s|$|;)|--chdir|(^|\s)-[a-zA-Z]*[CD]`)
})

// tempWriteAllowed reports a fixture write: p and its target in a temp folder, no live root, cwd move or link.
func tempWriteAllowed(cmd, p string) bool {
	return !movesCwd().MatchString(cmd) && !strings.Contains(cmd, "truncate") && !symlinkMaker().MatchString(cmd) &&
		tempRemovable(p) && tempRemovable(paths.Real(p))
}

// tempGitRemoval reports a plain `rm` whose operands and their targets pass tempWriteAllowed's rules (a fixture .git).
func tempGitRemoval(seg, cmd, cwd string) bool {
	seg, cmd = expandWith(seg, varPaths(cmd)), expandVars(cmd) // `T=/tmp/x && rm -rf $T/.git` removes a temp copy
	toks := shell.Tokens(seg)
	if len(toks) == 0 || toks[0] != "rm" || !removalAllowed(cmd, core.RemovedPaths(cmd, cwd)) {
		return false
	}
	removed := core.RemovedPaths(seg, cwd)
	return len(removed) > 0 && !slices.ContainsFunc(removed, func(p string) bool { return !tempWriteAllowed(cmd, p) })
}

// tempRemovable reports whether deleting p (the final element, never what it points at) touches only a temp folder.
func tempRemovable(p string) bool {
	if !filepath.IsAbs(p) || strings.ContainsAny(p, "*?[]{}$`") || slices.Contains(strings.Split(p, "/"), "..") {
		return false
	}
	target := filepath.Join(paths.CanonDeep(filepath.Dir(p)), filepath.Base(p))
	for _, live := range liveRoots() {
		if paths.Within(paths.FoldCase(target), paths.FoldCase(live)) {
			return false
		}
	}
	return underTemp(target)
}

// liveRoots are the trees a removal must never reach even when they sit under a temp root: the config dir, ~/.claude
// and the home directory, each symlink-resolved.
func liveRoots() []string {
	roots := []string{paths.Real(paths.ConfigDir())}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, paths.Real(filepath.Join(home, ".claude")), paths.Real(home))
	}
	return roots
}

// underTemp reports whether the resolved target sits under os.TempDir(), /tmp, /private/tmp or the macOS user temp.
func underTemp(target string) bool {
	if runtime.GOOS == "darwin" && darwinUserTemp().MatchString(target) {
		return true
	}
	for _, root := range []string{os.TempDir(), "/tmp", "/private/tmp"} {
		for _, r := range []string{filepath.Clean(root), paths.Real(root)} {
			if r != "/" && paths.Within(target, r) {
				return true
			}
		}
	}
	return false
}
