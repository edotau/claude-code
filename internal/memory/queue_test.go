package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	if !requeueDead() || pending() != 0 || !childAlive(dead) {
		t.Fatal("a live child was replayed")
	}
	write(t, childPath(dead), strconv.Itoa(os.Getpid())+" stale\n")
	if requeueDead() || pending() != 1 {
		t.Fatal("finished child was not requeued")
	}
}
