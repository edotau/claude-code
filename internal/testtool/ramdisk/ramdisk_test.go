package ramdisk

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDisk stubs the mount seams and reports what ran; attachErr makes the disk unavailable.
type fakeDisk struct {
	tmp      string
	attached []string
	detached []string
}

func stubDisk(t *testing.T, system string, attachErr error) *fakeDisk {
	t.Helper()
	f := &fakeDisk{tmp: t.TempDir()}
	origGoos, origAttach, origDetach := goos, attach, detach
	goos = system
	attach = func(volume string) (string, string, error) {
		f.attached = append(f.attached, volume)
		return "/dev/disk9", f.tmp, attachErr
	}
	detach = func(dev string) { f.detached = append(f.detached, dev) }
	t.Cleanup(func() { goos, attach, detach = origGoos, origAttach, origDetach })
	return f
}

// probe writes the child's TMPDIR to out and exits with code.
func probe(out, code string) []string {
	return []string{"sh", "-c", `printf %s "$TMPDIR" > "$0"; exit "$1"`, out, code}
}

func TestRunOnDiskPassesTMPDIRAndExitCodeThenDetaches(t *testing.T) {
	f := stubDisk(t, "darwin", nil)
	out := filepath.Join(t.TempDir(), "tmpdir")
	if code := Run(append([]string{"--"}, probe(out, "3")...)); code != 3 {
		t.Fatalf("exit = %d, want the command's 3", code)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != f.tmp {
		t.Errorf("child TMPDIR = %q, want %q", got, f.tmp)
	}
	if len(f.attached) != 1 || !strings.HasPrefix(f.attached[0], "claude-code-test-") {
		t.Errorf("attached = %v, want one per-run volume", f.attached)
	}
	if len(f.detached) != 1 || f.detached[0] != "/dev/disk9" {
		t.Errorf("detached = %v, want the attached device once", f.detached)
	}
}

func TestRunWithoutDiskUsesNormalTMPDIR(t *testing.T) {
	cases := []struct {
		name       string
		goos, off  string
		attachErr  error
		wantAttach int
	}{
		{name: "not macOS", goos: "linux"},
		{name: "opted out", goos: "darwin", off: "0"},
		{name: "attach failed", goos: "darwin", attachErr: errors.New("no hdiutil"), wantAttach: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := stubDisk(t, c.goos, c.attachErr)
			t.Setenv(offEnv, c.off)
			t.Setenv("TMPDIR", t.TempDir())
			out := filepath.Join(t.TempDir(), "tmpdir")
			if code := Run(probe(out, "0")); code != 0 {
				t.Fatalf("exit = %d", code)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != os.Getenv("TMPDIR") {
				t.Errorf("child TMPDIR = %q, want the inherited %q", got, os.Getenv("TMPDIR"))
			}
			if len(f.attached) != c.wantAttach || len(f.detached) != 0 {
				t.Errorf("attached %d, detached %d; want %d, 0", len(f.attached), len(f.detached), c.wantAttach)
			}
		})
	}
}

func TestRunErrorPaths(t *testing.T) {
	f := stubDisk(t, "darwin", nil)
	if code := Run(nil); code != exitUsage {
		t.Errorf("no command: exit = %d, want %d", code, exitUsage)
	}
	if len(f.attached) != 0 {
		t.Errorf("no command still attached a disk: %v", f.attached)
	}
	if code := Run([]string{"claude-code-no-such-command"}); code != exitNotFound {
		t.Errorf("missing command: exit = %d, want %d", code, exitNotFound)
	}
	if len(f.detached) != 1 {
		t.Errorf("missing command leaked the disk: detached = %v", f.detached)
	}
}
