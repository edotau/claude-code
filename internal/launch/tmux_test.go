package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeedTmuxConfSkipsNonENOENTProbeFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)        // os.UserHomeDir on unix
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on windows
	xdg := filepath.Join(home, "xdg-loop")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	probeSymlinkLoop(t, xdg)
	var out bytes.Buffer
	seedTmuxConf(&out, false)
	if _, err := os.Lstat(filepath.Join(home, ".tmux.conf")); !os.IsNotExist(err) {
		t.Fatalf("tmux seeded config despite failed XDG probe: Lstat = %v", err)
	}
	if !strings.Contains(out.String(), "skip") || !strings.Contains(out.String(), xdg) {
		t.Fatalf("tmux output = %q, want skip naming failed probe", out.String())
	}
}

func TestTmuxConfPathsEmptyWithoutHomeAndSeedSkips(t *testing.T) {
	t.Setenv("HOME", "")        // os.UserHomeDir on unix
	t.Setenv("USERPROFILE", "") // os.UserHomeDir on windows
	if got := tmuxConfPaths(); len(got) != 0 {
		t.Fatalf("tmuxConfPaths() = %v, want none when HOME is unresolved", got)
	}
	cwd := t.TempDir()
	t.Chdir(cwd)
	var out bytes.Buffer
	seedTmuxConf(&out, false)
	if !strings.Contains(out.String(), "skip") {
		t.Errorf("seedTmuxConf output = %q, want a skip line", out.String())
	}
	if _, err := os.Stat(filepath.Join(cwd, ".tmux.conf")); err == nil {
		t.Error("seedTmuxConf wrote .tmux.conf into the working directory")
	}
}
