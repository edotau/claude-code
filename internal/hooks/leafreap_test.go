//go:build unix

package hooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc/inflight"
)

// sleeper starts `sh -c 'sleep 30'` in its own process group writing to out; done closes once it is reaped.
func sleeper(t *testing.T, out string) <-chan struct{} {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	cmd := exec.Command("sh", "-c", "sleep 30")
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	return done
}

func stopped(done <-chan struct{}, within time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

// reapFixture returns a leaf stop payload whose transcript names only task A, and both tasks' sleepers.
func reapFixture(t *testing.T, extra ...string) (stop string, a, b <-chan struct{}) {
	t.Helper()
	if inflight.SessionOwner() == 0 {
		t.Skip("no session owner to retire against")
	}
	dir := filepath.Join(t.TempDir(), "tasks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outA := filepath.Join(dir, "bAAA.output")
	result := "Command running in background with ID: bAAA. Output is being written to: " + outA + ". You will be notified"
	tx := leafEnv(t, append([]string{result}, extra...)...)
	a, b = sleeper(t, outA), sleeper(t, filepath.Join(dir, "bBBB.output"))
	time.Sleep(200 * time.Millisecond) // let sh exec before the fd scan
	return fmt.Sprintf(`{"agent_id":"a1","transcript_path":%q,"agent_transcript_path":%q}`, tx, tx), a, b
}

// Given a leaf transcript naming task A, when the stop passes, A's group dies and unnamed task B's survives.
func TestSubagentStopReapsOwnBackgroundOnly(t *testing.T) {
	stop, a, b := reapFixture(t)
	subagentStopRunner(ParseInput(strings.NewReader(stop)))
	if !stopped(a, 5*time.Second) {
		t.Fatal("task A's process survived the stop")
	}
	if stopped(b, 200*time.Millisecond) {
		t.Fatal("task B's process (not in the leaf's transcript) was killed")
	}
}

// Given a running dispatch the leaf started, a blocked stop reaps nothing.
func TestSubagentStopBlockedReapsNothing(t *testing.T) {
	stop, a, _ := reapFixture(t, startedLine())
	writeDispatch(t, leafJob, record(leafJob, "running", os.Getpid()))
	if r := subagentStopRunner(ParseInput(strings.NewReader(stop))); r.Decision != Block {
		t.Fatalf("decision = %v, want Block", r.Decision)
	}
	if stopped(a, 300*time.Millisecond) {
		t.Fatal("a blocked stop killed a background process")
	}
}
