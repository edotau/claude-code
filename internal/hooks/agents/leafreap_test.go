package agents

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/proc"
)

// sleeper starts `sh -c 'exec sleep 30'` in its own process group writing to out; done closes once it is reaped.
func sleeper(t *testing.T, out string) <-chan struct{} {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", "exec sleep 30")
	cmd.Stdout, cmd.Stderr = f, f
	proc.OwnGroup(cmd) // cancel kills the whole group
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(cancel)
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
	if runtime.GOOS == "windows" {
		t.Skip("the reaper signals POSIX process groups")
	}
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
	return fmt.Sprintf(`{"agent_id":"a1","transcript_path":%q,"agent_transcript_path":%q}`, tx, tx), a, b
}

// Given a leaf transcript naming task A, when the stop passes, A's group dies and unnamed task B's survives.
func TestSubagentStopReapsOwnBackgroundOnly(t *testing.T) {
	stop, a, b := reapFixture(t)
	Stop(core.ParseInput(strings.NewReader(stop)))
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
	if r := Stop(core.ParseInput(strings.NewReader(stop))); r.Decision != core.Block {
		t.Fatalf("decision = %v, want Block", r.Decision)
	}
	if stopped(a, 300*time.Millisecond) {
		t.Fatal("a blocked stop killed a background process")
	}
}

// reapGroups never targets our own group, the parent session's, init's (<=1), our own pid's or a failed probe's,
// and lists each remaining group once.
func TestReapGroupsSkipsProtectedAndDuplicates(t *testing.T) {
	origOwn, origOf := reapOwnGroup, reapGroupOf
	t.Cleanup(func() { reapOwnGroup, reapGroupOf = origOwn, origOf })
	groups := map[int]int{os.Getppid(): 200, 11: 100, 12: 200, 13: 1, 14: 300, 15: 300, os.Getpid(): 400, 17: 500}
	reapOwnGroup = func() int { return 100 }
	reapGroupOf = func(pid int) (int, error) {
		if g, ok := groups[pid]; ok {
			return g, nil
		}
		return 0, syscall.ESRCH
	}
	got := reapGroups([]int{11, 12, 13, 14, 15, 16, os.Getpid(), 17})
	if want := []int{300, 500}; !slices.Equal(got, want) {
		t.Fatalf("reapGroups = %v, want %v", got, want)
	}
}

// terminateGroups sends SIGTERM, polls through the grace period, then SIGKILLs only when a group outlived it.
func TestTerminateGroupsEscalatesToKill(t *testing.T) {
	origSignal, origSleep := reapSignal, reapSleep
	t.Cleanup(func() { reapSignal, reapSleep = origSignal, origSleep })
	reapSleep = func(time.Duration) {}
	for _, tc := range []struct {
		name     string
		stubborn bool
		want     []string
	}{
		{"dies on TERM", false, []string{"7:15", "8:15"}},
		{"outlives grace", true, []string{"7:15", "8:15", "7:9", "8:9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []string
			reapSignal = func(g int, sig syscall.Signal) error {
				if sig == 0 {
					if tc.stubborn {
						return nil
					}
					return syscall.ESRCH
				}
				sent = append(sent, fmt.Sprintf("%d:%d", g, sig))
				return nil
			}
			terminateGroups([]int{7, 8})
			if !slices.Equal(sent, tc.want) {
				t.Fatalf("signals = %v, want %v", sent, tc.want)
			}
		})
	}
}
