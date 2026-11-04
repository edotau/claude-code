package statusline

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
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

// A planted symlink at the old tmp stamp path must not be followed: stamps live in the state dir now.
func TestWriteStampIgnoresLegacyTmpSymlink(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte("k"))
	old := filepath.Join(tmp, ".claude-statusline-tier-"+strconv.Itoa(os.Getuid())+"-"+strconv.FormatUint(h.Sum64(), 16))
	if err := os.Symlink(victim, old); err != nil {
		t.Fatal(err)
	}
	p := stampFile("tier", "k")
	writeStamp(p, "a")
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("victim overwritten: %q", b)
	}
	di, derr := os.Stat(filepath.Dir(p))
	fi, ferr := os.Stat(p)
	if derr != nil || ferr != nil || di.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
		t.Errorf("stamp dir/file = %v %v (%v, %v), want 0700/0600", di, fi, derr, ferr)
	}
	if !strings.HasPrefix(p, paths.StateDir()+string(filepath.Separator)) {
		t.Errorf("stamp %s outside state dir", p)
	}
}
