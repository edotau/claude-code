package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStoreGitProbesBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec \""+sleep+"\" 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	oldTimeout := gitTimeout
	gitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { gitTimeout = oldTimeout })

	t.Run("git toplevel", func(t *testing.T) {
		started := time.Now()
		if got := GitToplevel(t.TempDir()); got != "" {
			t.Errorf("timed-out toplevel probe = %q, want empty", got)
		}
		if elapsed := time.Since(started); elapsed >= time.Second {
			t.Errorf("toplevel probe took %v with timeout %v", elapsed, gitTimeout)
		}
	})
}
