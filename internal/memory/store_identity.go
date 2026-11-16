package memory

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// Identity is a cwd resolved to the canonical repository it belongs to; worktrees and subdirs share one.
type Identity struct {
	Origin string // canonical main checkout; "" for a non-repo cwd
}

// identities memoizes the git fork per cleaned root: a flip mid-process would split a store.
var identities sync.Map

// Resolve maps root to its canonical identity; any git failure is the home tier, never an error.
func Resolve(root string) Identity { return resolveWith(root, gitMain) }

// gitMain is root's main checkout per git, "" outside a repo; definitive is false when git itself failed.
func gitMain(root string) (top string, definitive bool) {
	definitive = true
	top = paths.MainCheckout(root, func(dir string, args ...string) (string, error) {
		out, err := gitOut(dir, args...)
		if err != nil && !gitRefused(err) {
			definitive = false
		}
		return out, err
	})
	return top, definitive
}

// gitRefused reports whether err is git running and exiting nonzero (a non-repo, no remote), not failing to run.
func gitRefused(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() > 0
}

// resolveWith is Resolve with main supplying root's git main checkout on a memo miss.
func resolveWith(root string, main func(string) (string, bool)) Identity {
	root = filepath.Clean(root)
	if paths.SamePath(root, paths.ConfigDir()) || paths.SamePath(paths.MainCheckout(root), paths.ConfigDir()) {
		return harnessIdentity()
	}
	origin, ok := identities.Load(root)
	if !ok {
		if id, fixed := fixedIdentity(root); fixed {
			return id
		}
		o, memoize := resolveOrigin(root, main)
		if !memoize {
			return Identity{}
		}
		origin = o
		identities.Store(root, origin)
	}
	o := origin.(string)
	switch {
	case o == "":
		return Identity{}
	case paths.SamePath(o, paths.ConfigDir()):
		return harnessIdentity()
	}
	return Identity{Origin: o}
}

// resolveOrigin finds root's origin; memoize is false when git failed to run, so a later call re-asks.
func resolveOrigin(root string, main func(string) (string, bool)) (origin string, memoize bool) {
	o, definitive := main(root)
	if o != "" {
		o = paths.CanonDeep(o)
	}
	return o, definitive || o != ""
}

// storeIdentity maps a cwd inside the store to the home tier, else ok=false.
func storeIdentity(root string) (Identity, bool) {
	if !paths.Within(paths.CanonDeep(root), paths.CanonDeep(StoreDir())) {
		return Identity{}, false
	}
	return Identity{}, true
}

func harnessIdentity() Identity {
	o := paths.CanonDeep(paths.ConfigDir())
	return Identity{Origin: o}
}

// OriginEnv carries the launching workspace's store origin to child sessions (set by agent.EnvSpec.Environ).
const OriginEnv = "HARNESS_MEMORY_ORIGIN"

// throwawayRoots are folders whose git repos are scratch or fixtures, never a project of their own.
func throwawayRoots() []string {
	roots := []string{os.TempDir(), "/tmp", filepath.Join(paths.ConfigDir(), "state")}
	if home, err := os.UserHomeDir(); err == nil {
		// TMPDIR=$HOME (or /) would make every repo scratch
		roots = slices.DeleteFunc(roots, func(r string) bool { return paths.SamePath(r, home) || filepath.Dir(r) == r })
		roots = append(roots, filepath.Join(home, "worktrees", "wf-fixtures"))
	}
	return roots
}

// throwawayInTests opts a test into the rule; other tests' t.TempDir() repos live under /tmp and must stay projects.
var throwawayInTests = false

// throwaway reports whether root sits in a scratch or fixture folder, which must never mint a store.
func throwaway(root string) bool {
	if testing.Testing() && !throwawayInTests {
		return false
	}
	canon := paths.CanonDeep(root)
	for _, r := range throwawayRoots() {
		if paths.Within(canon, paths.CanonDeep(r)) {
			return true
		}
	}
	return false
}

// fixedIdentity covers roots that never get a store of their own: inside the store, or a scratch/fixture folder.
func fixedIdentity(root string) (Identity, bool) {
	if id, ok := storeIdentity(root); ok {
		return id, true
	}
	if throwaway(root) {
		return throwawayIdentity(), true
	}
	return Identity{}, false
}

// throwawayIdentity is the launching workspace from OriginEnv, else the home tier.
func throwawayIdentity() Identity {
	o := os.Getenv(OriginEnv)
	if o == "" || !filepath.IsAbs(o) {
		return Identity{}
	}
	if paths.SamePath(o, paths.ConfigDir()) {
		return harnessIdentity()
	}
	o = paths.CanonDeep(o)
	return Identity{Origin: o}
}

// LaunchOrigin is the origin a child launched from cwd inherits: OriginEnv wins; the home tier passes nothing.
func LaunchOrigin(cwd string) string {
	if o := os.Getenv(OriginEnv); o != "" {
		return o
	}
	return Resolve(cwd).Origin
}

// routeMatch reports whether query names raw as a whole word, with or without its leading dot.
func routeMatch(query, raw string) bool {
	name := strings.ToLower(strings.TrimPrefix(raw, "."))
	if len(name) < 4 {
		return false
	}
	return routeWord(query, strings.ToLower(raw)) || routeWord(query, name)
}

// routeWord uses the same separator set at both ends, so dotted names stay whole.
func routeWord(query, name string) bool {
	for offset := 0; offset <= len(query)-len(name); {
		i := strings.Index(query[offset:], name)
		if i < 0 {
			return false
		}
		i += offset
		end := i + len(name)
		if (i == 0 || !routeWordByte(query[i-1])) && (end == len(query) || !routeWordByte(query[end])) {
			return true
		}
		offset = i + 1
	}
	return false
}

// routeWordByte reports whether c continues a route word.
func routeWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-'
}
