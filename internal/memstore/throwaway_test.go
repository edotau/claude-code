package memstore

import (
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

// A git repo in a scratch folder never becomes its own store: it takes the launching workspace, else home.
func TestThrowawayRepoUsesLaunchingWorkspace(t *testing.T) {
	tempConfig(t)
	orig := throwawayInTests
	throwawayInTests = true
	t.Cleanup(func() { throwawayInTests = orig })
	scratch := gitRepo(t, t.TempDir(), "p4-opencode-fix1")

	t.Setenv(OriginEnv, "")
	if id := Resolve(scratch); !id.HomeBank {
		t.Errorf("no %s: Resolve = %+v, want the home tier", OriginEnv, id)
	}
	ws := paths.CanonDeep(t.TempDir())
	t.Setenv(OriginEnv, ws)
	if id := Resolve(scratch); id.HomeBank || id.Origin != ws {
		t.Errorf("%s=%s: Resolve = %+v, want that workspace", OriginEnv, ws, id)
	}
	if got := LaunchOrigin(scratch); got != ws {
		t.Errorf("LaunchOrigin = %q, want the inherited %q", got, ws)
	}
}
