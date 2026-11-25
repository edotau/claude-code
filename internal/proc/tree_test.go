package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const treeModeEnv = "PROC_TREE_MODE"

// TestTreeHelper is the re-exec'd process: "parent" starts a detached "child" and a "middle" that double-forks one.
func TestTreeHelper(t *testing.T) {
	dir := os.Getenv("PROC_TREE_DIR")
	start := func(mode string) *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=^TestTreeHelper$", "--", "proc-tree-mark-"+filepath.Base(dir))
		c.Env = append(os.Environ(), treeModeEnv+"="+mode)
		setsid(c) // its own session, as Pi's bash is: a group kill of the parent does not reach it
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	switch os.Getenv(treeModeEnv) {
	case "child":
		time.Sleep(1500 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(dir, "late-"+strconv.Itoa(os.Getpid())), nil, 0o644)
	case "middle":
		start("child") // exits at once: the child reparents away from the tree, as a setsid daemon does
	case "parent":
		start("child")
		_ = start("middle").Wait()
		_ = os.WriteFile(filepath.Join(dir, "ready"), nil, 0o644)
		time.Sleep(30 * time.Second)
	}
}

// TestDetachDropsTreeTag: a daemon spawned through Detach (proxy, job) must outlive a TreeCancel sweep.
func TestDetachDropsTreeTag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Detach is a no-op off unix")
	}
	cmd := exec.Command("true")
	cmd.Env = []string{"A=1", TreeTagPrefix + "abc=1", "B=2"}
	Detach(cmd)
	if want := []string{"A=1", "B=2", detachedMarker + "=1"}; !slices.Equal(cmd.Env, want) {
		t.Fatalf("Detach env = %q, want %q", cmd.Env, want)
	}
}

func TestDetachNilEnvKeepsInherited(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Detach is a no-op off unix")
	}
	t.Setenv("HOME", "/h")
	t.Setenv(TreeTagPrefix+"x", "1")
	cmd := exec.Command("true")
	Detach(cmd)
	has := func(kv string) bool { return slices.Contains(cmd.Env, kv) }
	if !has("HOME=/h") || !has(detachedMarker+"=1") || !slices.ContainsFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		t.Fatalf("Detach nil env lost inherited vars: %d entries", len(cmd.Env))
	}
	if slices.ContainsFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, TreeTagPrefix) }) {
		t.Fatal("tree tag kept")
	}
}

// A child left in the group after its leader dies (a fork the cancel sweep missed) is gone once ReapGroup returns.
func TestReapGroupKillsLeftoverChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sh; taskkill /T takes the tree")
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.CommandContext(context.Background(), "sh", "-c", `sleep 30 & echo $! > "$1"; wait`, "sh", pidFile)
	TreeCancel(cmd, 10*time.Millisecond)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := 0
	for end := time.Now().Add(2 * time.Second); child == 0; time.Sleep(5 * time.Millisecond) {
		if b, _ := os.ReadFile(pidFile); len(b) > 0 && b[len(b)-1] == '\n' {
			child, _ = strconv.Atoi(string(b[:len(b)-1]))
		} else if time.Now().After(end) {
			t.Fatal("shell never started its sleep")
		}
	}
	_ = cmd.Process.Kill() // the leader only: the sleep stays in its group
	_ = cmd.Wait()
	if !ReapGroup(cmd.Process.Pid, time.Second) {
		t.Fatal("ReapGroup timed out with the group still alive")
	}
	for end := time.Now().Add(500 * time.Millisecond); Alive(child); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			_ = exec.Command("kill", "-9", strconv.Itoa(child)).Run()
			t.Fatalf("child %d of the reaped group survived", child)
		}
	}
}

func TestTreeCancelKillsDetachedDescendants(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTreeHelper$")
	cmd.Env = append(os.Environ(), treeModeEnv+"=parent", "PROC_TREE_DIR="+dir)
	TreeCancel(cmd, 100*time.Millisecond)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		} else if time.Now().After(end) {
			t.Fatal("helper never started its child")
		}
	}
	cancel()
	_ = cmd.Wait()
	time.Sleep(2 * time.Second)
	if late, _ := filepath.Glob(filepath.Join(dir, "late-*")); len(late) > 0 {
		t.Fatalf("%d detached or reparented descendant(s) outlived the cancelled run: %v", len(late), late)
	}
}
