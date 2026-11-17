package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestQueueDuplicateEnqueueCoalesces(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for i, want := range []bool{true, false} {
		got, err := Enqueue(Job{Kind: "update", Key: "s/1"})
		if err != nil || got != want {
			t.Fatalf("enqueue %d = %v, %v; want %v, nil", i, got, err, want)
		}
	}
	if n := pending(); n != 1 {
		t.Fatalf("pending = %d, want 1", n)
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
	if runtime.GOOS == "windows" {
		t.Skip("process liveness is not probed off unix")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pid := deadPID(t)
	runDir := filepath.Join(queueDir(), "running")
	for want := 1; want <= queueMaxAttempts; want++ {
		if _, err := Enqueue(Job{Kind: "update", Key: "s", Attempts: want - 1}); err != nil {
			t.Fatal(err)
		}
		_, path, ok := claimNext(time.Now())
		if !ok {
			t.Fatal("nothing claimed")
		}
		if err := os.Rename(path, filepath.Join(runDir, "update-s.json."+strconv.Itoa(pid))); err != nil {
			t.Fatal(err)
		}
		requeueDead()
		if want < queueMaxAttempts {
			j, ok := readJob(filepath.Join(queueDir(), "update-s.json"))
			if !ok || j.Attempts != want {
				t.Fatalf("after death %d: job = %+v, ok = %v; want attempts %d", want, j, ok, want)
			}
			_ = os.Remove(filepath.Join(queueDir(), "update-s.json"))
		}
	}
	if _, err := os.Stat(filepath.Join(queueDir(), "failed", "update-s.json")); err != nil || pending() != 0 {
		t.Fatalf("third death should land in failed/: stat err %v, pending %d", err, pending())
	}
}
