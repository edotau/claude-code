//go:build unix

package configure

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/gemini"
)

// TestRunAgyStopsDescendants: a non-exec shell spawning a background child (stand-in for Node spawning a
// child process) must have its descendant killed by stopAgy when the sign-in marker appears.
func TestRunAgyStopsDescendants(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	marker := gemini.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pidFileSh := filepath.Join(dir, "sh.pid")
	pidFile := filepath.Join(dir, "child.pid")
	// Marker last: a slow runner's poll must not kill the shell before the child and its pidfile exist.
	script := "echo $$ > '" + pidFileSh + "'; sleep 30 & echo $! > '" + pidFile + "'; sleep 0.1; echo tok > '" + marker + "'; wait"
	fakeAgyCommand(t, script)
	origPoll, origGrace := agySignInPoll, agyStopGrace
	t.Cleanup(func() { agySignInPoll, agyStopGrace = origPoll, origGrace })
	agySignInPoll, agyStopGrace = 50*time.Millisecond, 50*time.Millisecond

	var out bytes.Buffer
	d := &dialog{out: &out}
	if !runAgy(d) {
		t.Fatalf("runAgy = false, want true: %s", out.String())
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pidfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid child pid %q: %v", raw, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("child pid %d still alive after runAgy returned", pid)
}
