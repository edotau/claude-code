package memory

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/providers"
)

func TestQueueDuplicateEnqueueCoalesces(t *testing.T) {
	for _, keys := range [][]string{{"s/1"}, {"s/1", "s_1", "S/1"}} {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		for _, key := range keys {
			for i, want := range []bool{true, false} {
				got, err := enqueue(job{Kind: "update", Key: key})
				if err != nil || got != want {
					t.Fatalf("enqueue %q %d = %v, %v; want %v, nil", key, i, got, err, want)
				}
			}
		}
		if n := pending(); n != len(keys) {
			t.Fatalf("pending = %d, want %d", n, len(keys))
		}
	}
	if _, err := enqueue(job{Kind: "bogus", Key: "k"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestQueueClaimOrderNotBeforeAndFinish(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now().UTC()
	jobs := []job{
		{Kind: "dream", Key: "later", Enqueued: now.Add(-time.Minute), NotBefore: now.Add(time.Hour)},
		{Kind: "update", Key: "b", Enqueued: now.Add(-time.Second)},
		{Kind: "update", Key: "a", Enqueued: now.Add(-time.Minute)},
	}
	for _, j := range jobs {
		if _, err := enqueue(j); err != nil {
			t.Fatal(err)
		}
	}
	var order []string
	for {
		j, path, ok := claimNext(now)
		if !ok {
			break
		}
		order = append(order, j.Key)
		if err := finish(path); err != nil {
			t.Fatal(err)
		}
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" || pending() != 1 {
		t.Fatalf("order = %v, pending = %d; want [a b] and the delayed job left", order, pending())
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=NONE")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestQueueDeadWorkerRequeuesThenFails(t *testing.T) {
	for _, pid := range []int{deadPID(t), os.Getpid()} {
		t.Run(strconv.Itoa(pid), func(t *testing.T) { checkQueueDeadWorker(t, pid) })
	}
}

func checkQueueDeadWorker(t *testing.T, pid int) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	runDir := filepath.Join(queueDir(), "running")
	for want := 1; want <= queueMaxAttempts; want++ {
		if _, err := enqueue(job{Kind: "update", Key: "s", Attempts: want - 1}); err != nil {
			t.Fatal(err)
		}
		_, path, ok := claimNext(time.Now())
		if !ok {
			t.Fatal("nothing claimed")
		}
		if err := os.Rename(path, filepath.Join(runDir, jobName("update", "s")+"."+strconv.Itoa(pid)+".dead.unique")); err != nil {
			t.Fatal(err)
		}
		requeueDead()
		if want < queueMaxAttempts {
			j, ok := readJob(filepath.Join(queueDir(), jobName("update", "s")))
			if !ok || j.Attempts != want {
				t.Fatalf("after death %d: job = %+v, ok = %v; want attempts %d", want, j, ok, want)
			}
			_ = os.Remove(filepath.Join(queueDir(), jobName("update", "s")))
		}
	}
	if _, err := os.Stat(filepath.Join(queueDir(), "failed", jobName("update", "s"))); err != nil || pending() != 0 {
		t.Fatalf("third death should land in failed/: stat err %v, pending %d", err, pending())
	}
}

func TestQueueDeadWorkerWaitsForLiveChild(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := enqueue(job{Kind: "dream", Key: "today"}); err != nil {
		t.Fatal(err)
	}
	_, path, ok := claimNext(time.Now())
	if !ok {
		t.Fatal("nothing claimed")
	}
	dead := filepath.Join(filepath.Dir(path), jobName("dream", "today")+".999999.dead.unique")
	if err := os.Rename(path, dead); err != nil {
		t.Fatal(err)
	}
	start, err := proc.StartTime(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	write(t, childPath(dead), strconv.Itoa(os.Getpid())+" "+start+"\n")
	requeueDead()
	if pending() != 0 || !childAlive(dead) {
		t.Fatal("a live child was replayed")
	}
	write(t, childPath(dead), strconv.Itoa(os.Getpid())+" stale\n")
	requeueDead()
	if pending() != 1 {
		t.Fatal("finished child was not requeued")
	}
}

func TestQueueDeadWorkerBeforeChildIdentityHonorsLease(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := enqueue(job{Kind: "dream", Key: "today"}); err != nil {
		t.Fatal(err)
	}
	_, path, ok := claimNext(time.Now())
	if !ok {
		t.Fatal("nothing claimed")
	}
	dead := filepath.Join(filepath.Dir(path), jobName("dream", "today")+".999999.dead.unique")
	if err := os.Rename(path, dead); err != nil {
		t.Fatal(err)
	}
	write(t, childPath(dead), "0 "+strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)+"\n")
	requeueDead()
	if pending() != 0 {
		t.Fatal("a job inside its spawn lease was replayed")
	}
	write(t, childPath(dead), "0 "+strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)+"\n")
	requeueDead()
	if pending() != 1 {
		t.Fatal("an expired lease was not requeued")
	}
}

// BenchmarkStartWorkersQueue is the session-start worker wake check against waiting jobs.
func BenchmarkStartWorkersQueue(b *testing.B) {
	for _, tc := range []struct {
		name          string
		future, ready int
	}{{"empty", 0, 0}, {"one-due", 0, 1}, {"200-future", 200, 0}, {"200-future-one-due", 200, 1}} {
		b.Run(tc.name, func(b *testing.B) {
			b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
			now := time.Now().UTC()
			for i := range tc.future {
				_, _ = enqueue(job{Kind: "update", Key: "f" + strconv.Itoa(i), NotBefore: now.Add(time.Hour)})
			}
			for i := range tc.ready {
				_, _ = enqueue(job{Kind: "update", Key: "r" + strconv.Itoa(i)})
			}
			orig := spawn.catchup
			spawn.catchup = func(string, string) bool { return true }
			b.Cleanup(func() { spawn.catchup = orig })
			b.ResetTimer()
			for range b.N {
				startWorkers("/tmp", "probe", now, false)
			}
		})
	}
}

// A failing identity removal must not leave the job behind to be replayed.
func TestQueueFinishRemovesJobBeforeIdentity(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	job := filepath.Join(queueDir(), "running", "x.json.1.a.b")
	write(t, job, "{}")
	write(t, filepath.Join(childPath(job), "pin"), "x") // a non-empty directory cannot be removed
	if err := finish(job); err == nil {
		t.Fatal("finish ignored the identity removal failure")
	}
	if _, err := os.Stat(job); !os.IsNotExist(err) {
		t.Fatalf("job survived a failed identity removal: %v", err)
	}
}

func TestAwaitGateNeedsGoByte(t *testing.T) {
	t.Setenv(queueGateEnv, "1")
	if awaitGate(strings.NewReader("")) {
		t.Fatal("a closed gate released the child")
	}
	if !awaitGate(strings.NewReader("1")) || os.Getenv(queueGateEnv) != "" {
		t.Fatal("go byte did not release the child or the env stayed set")
	}
}

// One live orphan must not hold back an unrelated due job.
func TestQueueDrainSkipsLiveOrphan(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := enqueue(job{Kind: "dream", Key: "orphan"}); err != nil {
		t.Fatal(err)
	}
	_, path, _ := claimNext(time.Now())
	dead := filepath.Join(filepath.Dir(path), jobName("dream", "orphan")+".999999.dead.unique")
	if err := os.Rename(path, dead); err != nil {
		t.Fatal(err)
	}
	start, _ := proc.StartTime(os.Getpid())
	write(t, childPath(dead), strconv.Itoa(os.Getpid())+" "+start+"\n")
	if _, err := enqueue(job{Kind: "update", Key: "other"}); err != nil {
		t.Fatal(err)
	}
	var ran []string
	orig := runQueuedJob
	runQueuedJob = func(_ context.Context, _ string, j job, _ string, _ io.Writer) error {
		ran = append(ran, j.Key)
		return nil
	}
	t.Cleanup(func() { runQueuedJob = orig })
	drainQueue(io.Discard)
	if len(ran) != 1 || ran[0] != "other" {
		t.Fatalf("ran %v, want only the unrelated job", ran)
	}
}

// A waiting job's mtime is its due time, so the due check only reads files that are due.
func TestQueuedDueUsesMtime(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := enqueue(job{Kind: "update", Key: "later", NotBefore: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(queued()[0])
	if err != nil || !fi.ModTime().Equal(now.Add(time.Hour)) {
		t.Fatalf("mtime = %v, %v; want the not_before time", fi, err)
	}
	if queuedDue(now) || !queuedDue(now.Add(2*time.Hour)) {
		t.Fatal("due check ignores not_before")
	}
}

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

func TestWithoutRouteEnvDropsParentRoute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh to dump the child env")
	}
	tempConfig(t)
	for _, k := range routeEnv {
		t.Setenv(k, "parent-"+k)
	}
	t.Setenv("KEEP_ME", "yes")
	dump := filepath.Join(t.TempDir(), "env.txt")
	if !spawnDetached("/bin/sh", []string{"-c", "env > \"$1.tmp\" && mv \"$1.tmp\" \"$1\"", "sh", dump}, "", nil, "EXTRA=1") {
		t.Fatal("spawnDetached = false")
	}
	lines := readEnvDump(t, dump)
	for _, k := range routeEnv {
		if slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, k+"=") }) {
			t.Errorf("%s leaked into the child env", k)
		}
	}
	for _, want := range []string{"KEEP_ME=yes", "EXTRA=1"} {
		if !slices.Contains(lines, want) {
			t.Errorf("child env lost %s", want)
		}
	}
}

// readEnvDump waits for the detached child to write path and returns its lines.
func readEnvDump(t *testing.T, path string) []string {
	t.Helper()
	for range 500 {
		if got, err := os.ReadFile(path); err == nil {
			return strings.Split(string(got), "\n")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child never wrote its env")
	return nil
}

func TestSpawnSelfLaunchesWithLogUnderStateDir(t *testing.T) {
	tempConfig(t)
	got := captureLaunches(t)
	if !spawnSelf([]string{"memory", "x"}, "/work", "x.log", "A=1") {
		t.Fatal("spawnSelf = false with a recording launcher")
	}
	self, _ := os.Executable()
	want := launch{bin: self, dir: "/work", log: filepath.Join(paths.MemoryStateDir(), "x.log"), args: []string{"memory", "x"},
		env: []string{"A=1"}}
	if len(*got) != 1 || !sameLaunch((*got)[0], want) {
		t.Fatalf("launch count = %d; want one launch matching args %q", len(*got), want.args)
	}
	if st, err := os.Stat(want.log); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("log file stat = %v, %v; want an 0600 file", st, err)
	}
}

func TestDefaultSpawnersBuildTodaysArgvAndLogs(t *testing.T) {
	tempConfig(t)
	got := captureLaunches(t)
	spawnUpdate("/t.jsonl", "/c", "s1")
	spawnWatcher([]string{"--session", "s1", "--transcript", "/t.jsonl"}, "/c")
	spawnCatchup("/c", "s1")
	spawnEmbedRefresh("/r", "p1")
	stamp := `\d{8}-\d{6}\.\d{3}`
	want := []struct {
		args       []string
		dir, logRE string
		env        []string
	}{
		{[]string{"memory", "update", "--transcript", "/t.jsonl", "--cwd", "/c", "--session", "s1"}, "/c", "update-" + stamp + `\.log`, []string{DetachedEnv + "=1"}},
		{[]string{"memory", "watch", "--session", "s1", "--transcript", "/t.jsonl"}, paths.MemoryStateDir(), `watch-s1\.log`, nil},
		{[]string{"memory", "update", "--catchup", "--skip", "s1"}, "/c", "catchup-" + stamp + `\.log`, []string{DetachedEnv + "=1"}},
		{[]string{"memory", "index", "--mint-only"}, "/r", "",
			[]string{providers.EnvSessionProfile + "=p1", UpdateChildEnv + "=1"}},
	}
	if len(*got) != len(want) {
		t.Fatalf("launch count = %d; want %d", len(*got), len(want))
	}
	for i, w := range want {
		g := (*got)[i]
		if !slices.Equal(g.args, w.args) || g.dir != w.dir || !stateLog(g.log, w.logRE) || !slices.Equal(g.env, w.env) {
			t.Errorf("launch %d: args %q dir %q; want args %q dir %q log %q", i, g.args, g.dir, w.args, w.dir, w.logRE)
		}
	}
	if r, ok := readUpdateResult("s1"); !ok || r.Status != updatePending || r.Log != (*got)[0].log {
		t.Errorf("update result = %+v, %v; want pending with log %q", r, ok, (*got)[0].log)
	}
}

func TestStubSpawnsCountsAndRestoresNested(t *testing.T) {
	base := StubSpawn(t, true)
	outer, restoreOuter := StubSpawns(true)
	inner, restoreInner := StubSpawns(false)
	if spawn.update("", "", "") || !spawn.catchup("", "") || !spawn.watcher(nil, "") || !spawn.refresh("", "") {
		t.Fatal("inner stub: want update=false (ok flag), the rest true")
	}
	restoreInner()
	if !spawn.update("", "", "") {
		t.Fatal("after inner restore, update = false; want the outer stub's true")
	}
	restoreOuter()
	spawn.update("", "", "")
	counts := [3][3]int64{
		{inner.Update.Load(), inner.Catchup.Load(), inner.Refresh.Load()},
		{outer.Update.Load(), outer.Catchup.Load(), outer.Refresh.Load()},
		{base.Update.Load(), base.Catchup.Load(), base.Refresh.Load()},
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
	return filepath.Dir(path) == paths.MemoryStateDir() && regexp.MustCompile(`^`+re+`$`).MatchString(filepath.Base(path))
}

func sameLaunch(a, b launch) bool {
	return a.bin == b.bin && a.dir == b.dir && a.log == b.log && slices.Equal(a.args, b.args) && slices.Equal(a.env, b.env)
}

// StubSpawn is memtest.StubSpawn for this package's own tests, which cannot import memtest (cycle).
func StubSpawn(tb testing.TB, ok bool) *Spawns {
	tb.Helper()
	s, restore := StubSpawns(ok)
	tb.Cleanup(restore)
	return s
}
