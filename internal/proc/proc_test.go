package proc

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// The guard must recognise the running test binary, or a detached relaunch fork-bombs the suite.
func TestIsSelfUnderTest(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !IsSelfUnderTest(self) {
		t.Fatal("IsSelfUnderTest(os.Executable()) = false under go test")
	}
	if SpawnDetached(self, []string{"-test.run=^$"}, "", nil) {
		t.Fatal("SpawnDetached relaunched the test binary")
	}
	if IsSelfUnderTest("/bin/sh") {
		t.Fatal("IsSelfUnderTest(/bin/sh) = true")
	}
}

func TestStartTimeOfSelfIsStable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux /proc identity")
	}
	a, err := StartTime(os.Getpid())
	b, _ := StartTime(os.Getpid())
	if err != nil || a == "" || a != b {
		t.Fatalf("StartTime = %q, %q, %v; want non-empty and stable", a, b, err)
	}
	if _, err := StartTime(1 << 30); err == nil {
		t.Fatal("a missing pid must error")
	}
}

// Alive must report the calling process itself as alive, and a reaped child pid as dead.
func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("Alive(own pid) must be true")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn a short-lived process: %v", err)
	}
	if Alive(cmd.Process.Pid) {
		t.Error("Alive(reaped child pid) must be false")
	}
}
