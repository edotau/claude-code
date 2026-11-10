package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestArgvPreservesTrailingEmptyArguments(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux /proc argv")
	}
	cmd := exec.Command("sh", "-c", "read -r line", "value", "", "")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	var got []string
	// /proc cmdline can be briefly empty while the child finishes exec.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		got, err = Argv(cmd.Process.Pid)
		if len(got) > 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, cmd.Args) {
		t.Fatalf("Argv lost trailing empty arguments: got %q, want %q", got, cmd.Args)
	}
}

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

func TestExecRefusesUnderTest(t *testing.T) {
	orig := execImage
	var reached []string
	execImage = func(bin string, _, _ []string) error { reached = append(reached, bin); return nil }
	t.Cleanup(func() { execImage = orig })
	err := Exec("/usr/bin/true", []string{"x"}, nil)
	if !errors.Is(err, ErrExecUnderTest) {
		t.Fatalf("Exec under go test = %v, want ErrExecUnderTest", err)
	}
	if len(reached) != 0 {
		t.Fatalf("Exec reached the raw exec seam with %v under go test", reached)
	}
}

func TestStartTimeOfSelfIsStable(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("StartTime reads /proc (linux) or ps (darwin)")
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
	if runtime.GOOS == "windows" {
		t.Skip("Alive is a signal-0 probe; off unix it always reports false")
	}
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

// With out == nil the child's stdout/stderr must be /dev/null, not closed fds (a write must succeed).
func TestSpawnDetachedNilOutKeepsStdioOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	marker := filepath.Join(t.TempDir(), "rc")
	script := `echo hi >&1 2>/dev/null; a=$?; echo hi >&2 2>/dev/null; b=$?; echo "$a$b" > "$1"`
	if !SpawnDetached("/bin/sh", []string{"-c", script, "sh", marker}, "", nil) {
		t.Fatal("SpawnDetached failed to start /bin/sh")
	}
	var got []byte
	for range 100 {
		if got, _ = os.ReadFile(marker); len(got) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if string(got) != "00\n" {
		t.Fatalf("child write exit codes (stdout, stderr) = %q; want \"00\\n\" (fds closed instead of /dev/null)", got)
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
