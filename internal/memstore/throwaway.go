package memstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// OriginEnv carries the launching workspace's store origin to child sessions (set by agent.EnvSpec.Environ).
const OriginEnv = "HARNESS_MEMORY_ORIGIN"

// throwawayRoots are folders whose git repos are scratch or fixtures, never a project of their own.
func throwawayRoots() []string {
	roots := []string{os.TempDir(), "/tmp", filepath.Join(paths.ConfigDir(), "state")}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, filepath.Join(home, "worktrees", "wf-fixtures"))
	}
	return roots
}

// throwawayInTests opts a test into the rule; other tests' t.TempDir() repos live under /tmp and must stay projects.
var throwawayInTests = false

// Throwaway reports whether root sits in a scratch or fixture folder, which must never mint a store.
func Throwaway(root string) bool {
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
	if Throwaway(root) {
		return throwawayIdentity(), true
	}
	return Identity{}, false
}

// throwawayIdentity is the launching workspace from OriginEnv, else the home tier.
func throwawayIdentity() Identity {
	o := os.Getenv(OriginEnv)
	if o == "" || !filepath.IsAbs(o) {
		return Identity{HomeBank: true}
	}
	if paths.SamePath(o, paths.ConfigDir()) {
		return harnessIdentity()
	}
	o = paths.CanonDeep(o)
	return Identity{Origin: o, Base: filepath.Base(o)}
}

// LaunchOrigin is the origin a child launched from cwd inherits: OriginEnv wins; the home tier passes nothing.
func LaunchOrigin(cwd string) string {
	if o := os.Getenv(OriginEnv); o != "" {
		return o
	}
	if id := Resolve(cwd); !id.HomeBank {
		return id.Origin
	}
	return ""
}
