package statusline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

func TestStampLivesUnderStateDirAndRoundTrips(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	p := stampFile("tier", "k")
	if !strings.HasPrefix(p, paths.StateDir()+string(filepath.Separator)) {
		t.Fatalf("stamp %s is outside the state dir", p)
	}
	writeTimedStamp(p, "a", "b")
	if got := freshStamp(p, 1<<40, 2); len(got) != 2 || got[1] != "b" {
		t.Fatalf("freshStamp = %v", got)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("stamp mode = %v, %v", fi, err)
	}
}
