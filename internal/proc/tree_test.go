package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const treeModeEnv = "PROC_TREE_MODE"

// TestTreeHelper is the re-exec'd process: "parent" starts a detached "child", which writes the marker late.
func TestTreeHelper(t *testing.T) {
	dir := os.Getenv("PROC_TREE_DIR")
	switch os.Getenv(treeModeEnv) {
	case "child":
		time.Sleep(1500 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(dir, "late"), nil, 0o644)
	case "parent":
		c := exec.Command(os.Args[0], "-test.run=^TestTreeHelper$")
		c.Env = append(os.Environ(), treeModeEnv+"=child")
		Detach(c) // its own session, as Pi's bash is: a group kill of the parent does not reach it
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, "ready"), nil, 0o644)
		time.Sleep(30 * time.Second)
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
	if _, err := os.Stat(filepath.Join(dir, "late")); err == nil {
		t.Fatal("a detached descendant outlived the cancelled run and wrote its marker")
	}
}
