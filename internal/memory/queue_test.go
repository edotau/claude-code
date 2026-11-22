package memory

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

func TestQueueDuplicateEnqueueCoalesces(t *testing.T) {
	for _, keys := range [][]string{{"s/1"}, {"s/1", "s_1", "S/1"}} {
		t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
		for _, key := range keys {
			for i, want := range []bool{true, false} {
				got, err := Enqueue(Job{Kind: "update", Key: key})
				if err != nil || got != want {
					t.Fatalf("enqueue %q %d = %v, %v; want %v, nil", key, i, got, err, want)
				}
			}
		}
		if n := pending(); n != len(keys) {
			t.Fatalf("pending = %d, want %d", n, len(keys))
		}
	}
	if _, err := Enqueue(Job{Kind: "bogus", Key: "k"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestQueueClaimOrderNotBeforeAndFinish(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now().UTC()
	jobs := []Job{
		{Kind: "dream", Key: "later", Enqueued: now.Add(-time.Minute), NotBefore: now.Add(time.Hour)},
		{Kind: "update", Key: "b", Enqueued: now.Add(-time.Second)},
		{Kind: "update", Key: "a", Enqueued: now.Add(-time.Minute)},
	}
	for _, j := range jobs {
		if _, err := Enqueue(j); err != nil {
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
		if _, err := Enqueue(Job{Kind: "update", Key: "s", Attempts: want - 1}); err != nil {
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
	if _, err := Enqueue(Job{Kind: "dream", Key: "today"}); err != nil {
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
	if _, err := Enqueue(Job{Kind: "dream", Key: "today"}); err != nil {
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
				_, _ = Enqueue(Job{Kind: "update", Key: "f" + strconv.Itoa(i), NotBefore: now.Add(time.Hour)})
			}
			for i := range tc.ready {
				_, _ = Enqueue(Job{Kind: "update", Key: "r" + strconv.Itoa(i)})
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
	if _, err := Enqueue(Job{Kind: "dream", Key: "orphan"}); err != nil {
		t.Fatal(err)
	}
	_, path, _ := claimNext(time.Now())
	dead := filepath.Join(filepath.Dir(path), jobName("dream", "orphan")+".999999.dead.unique")
	if err := os.Rename(path, dead); err != nil {
		t.Fatal(err)
	}
	start, _ := proc.StartTime(os.Getpid())
	write(t, childPath(dead), strconv.Itoa(os.Getpid())+" "+start+"\n")
	if _, err := Enqueue(Job{Kind: "update", Key: "other"}); err != nil {
		t.Fatal(err)
	}
	var ran []string
	orig := runQueuedJob
	runQueuedJob = func(_ context.Context, _ string, j Job, _ string, _ io.Writer) error {
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
	if _, err := Enqueue(Job{Kind: "update", Key: "later", NotBefore: now.Add(time.Hour)}); err != nil {
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
