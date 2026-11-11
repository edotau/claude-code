package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc"
)

// A timed-out git whose grandchild keeps stdout open returns within the timeout plus WaitDelay, not when it exits.
func TestVerifyGitBoundsGrandchildHoldingStdout(t *testing.T) {
	oldDelay := proc.ExecWaitDelay
	proc.ExecWaitDelay = 300 * time.Millisecond
	t.Cleanup(func() { proc.ExecWaitDelay = oldDelay })
	bin := t.TempDir()
	script := "#!/bin/sh\nsleep 10 &\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	orig := verifyGitTimeout
	verifyGitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { verifyGitTimeout = orig })
	start := time.Now()
	if _, err := verifyGit(t.TempDir(), "", "status"); err == nil {
		t.Fatal("verifyGit succeeded despite the timeout")
	}
	if elapsed := time.Since(start); elapsed < proc.ExecWaitDelay || elapsed > verifyGitTimeout+proc.ExecWaitDelay+2*time.Second {
		t.Fatalf("verifyGit returned after %v; want between %v and %v",
			elapsed, proc.ExecWaitDelay, verifyGitTimeout+proc.ExecWaitDelay+2*time.Second)
	}
}
