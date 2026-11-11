package memory

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

type launch struct {
	bin, dir, log string
	args, env     []string
}

// captureLaunches swaps the process-launch seam for a recorder; log is the opened file's path, "" when none.
func captureLaunches(t *testing.T) *[]launch {
	t.Helper()
	var got []launch
	orig := spawnDetached
	spawnDetached = func(bin string, args []string, dir string, out *os.File, env ...string) bool {
		l := launch{bin: bin, dir: dir, args: args, env: env}
		if out != nil {
			l.log = out.Name()
		}
		got = append(got, l)
		return true
	}
	t.Cleanup(func() { spawnDetached = orig })
	return &got
}

func TestSpawnSelfLaunchesWithLogUnderStateDir(t *testing.T) {
	tempConfig(t)
	got := captureLaunches(t)
	if !spawnSelf([]string{"memory", "x"}, "/work", "x.log", "A=1") {
		t.Fatal("spawnSelf = false with a recording launcher")
	}
	self, _ := os.Executable()
	want := launch{bin: self, dir: "/work", log: filepath.Join(StateDir(), "x.log"), args: []string{"memory", "x"},
		env: []string{"A=1"}}
	if len(*got) != 1 || !sameLaunch((*got)[0], want) {
		t.Fatalf("launches = %+v; want [%+v]", *got, want)
	}
	if st, err := os.Stat(want.log); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("log file stat = %v, %v; want an 0600 file", st, err)
	}
}

func TestDefaultSpawnersBuildTodaysArgvAndLogs(t *testing.T) {
	tempConfig(t)
	got := captureLaunches(t)
	spawnUpdate("/t.jsonl", "/c", "s1")
	spawnWatcher([]string{"--session", "s1", "--transcript", "/t.jsonl"}, "/c")
	spawnDream("/c")
	spawnCatchup("/c", "s1")
	spawnEmbedRefresh("/r", "p1")
	stamp := `\d{8}-\d{6}`
	want := []struct {
		args       []string
		dir, logRE string
		env        []string
	}{
		{[]string{"memory", "update", "--transcript", "/t.jsonl", "--cwd", "/c", "--session", "s1"}, "/c", "update-" + stamp + `\.log`, nil},
		{[]string{"memory", "watch", "--session", "s1", "--transcript", "/t.jsonl"}, "/c", `watch-s1\.log`, nil},
		{[]string{"memory", "dream", "--cwd", "/c"}, "/c", "dream-" + stamp + `\.log`, nil},
		{[]string{"memory", "catchup", "--skip", "s1"}, "/c", "catchup-" + stamp + `\.log`, nil},
		{[]string{"memory", "index", "--mint-only", "--repo", "/r"}, "/r", "",
			[]string{providers.EnvSessionProfile + "=p1", UpdateChildEnv + "=1"}},
	}
	if len(*got) != len(want) {
		t.Fatalf("launches = %+v; want %d", *got, len(want))
	}
	for i, w := range want {
		g := (*got)[i]
		if !slices.Equal(g.args, w.args) || g.dir != w.dir || !stateLog(g.log, w.logRE) || !slices.Equal(g.env, w.env) {
			t.Errorf("launch %d = %+v; want args %q dir %q log %q env %q", i, g, w.args, w.dir, w.logRE, w.env)
		}
	}
	if r, ok := readUpdateResult("s1"); !ok || r.Status != updatePending || r.Log != (*got)[0].log {
		t.Errorf("update result = %+v, %v; want pending with log %q", r, ok, (*got)[0].log)
	}
}

func TestStubSpawnsCountsAndRestoresNested(t *testing.T) {
	base := StubSpawn(t, true)
	outer, restoreOuter := stubSpawns(true)
	inner, restoreInner := stubSpawns(false)
	if spawn.update("", "", "") || !spawn.dream("") || !spawn.catchup("", "") || !spawn.watcher(nil, "") {
		t.Fatal("inner stub: want update=false (ok flag), the rest true")
	}
	restoreInner()
	if !spawn.update("", "", "") {
		t.Fatal("after inner restore, update = false; want the outer stub's true")
	}
	restoreOuter()
	spawn.update("", "", "")
	counts := [3][3]int64{
		{inner.Update.Load(), inner.Dream.Load(), inner.Catchup.Load()},
		{outer.Update.Load(), outer.Dream.Load(), outer.Catchup.Load()},
		{base.Update.Load(), base.Dream.Load(), base.Catchup.Load()},
	}
	if counts != [3][3]int64{{1, 1, 1}, {1, 0, 0}, {1, 0, 0}} {
		t.Fatalf("inner/outer/base counts = %v; want each restore to reinstate the previous stub", counts)
	}
}

// stateLog reports path is a state-dir file whose name matches re; an empty re wants no log at all.
func stateLog(path, re string) bool {
	if re == "" {
		return path == ""
	}
	return filepath.Dir(path) == StateDir() && regexp.MustCompile(`^`+re+`$`).MatchString(filepath.Base(path))
}

func sameLaunch(a, b launch) bool {
	return a.bin == b.bin && a.dir == b.dir && a.log == b.log && slices.Equal(a.args, b.args) && slices.Equal(a.env, b.env)
}
