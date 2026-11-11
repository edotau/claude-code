package browser

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/pathview"
)

func TestMain(m *testing.M) {
	hosttest.Tools = append(append([]string(nil), hosttest.Tools...), "node")
	os.Exit(hosttest.Main(m))
}

func TestRunReal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	out, err := runReal("sh", "-c", "printf '  detected\\n'")
	if err != nil || out != "detected" {
		t.Fatalf("runReal = %q, %v", out, err)
	}
	if _, err := runReal("sh", "-c", "exit 7"); err == nil {
		t.Fatal("runReal ignored command failure")
	}
}

func TestRunCommandBoundsWait(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	start := time.Now()
	if _, err := runCommand(50*time.Millisecond, "sh", "-c", "while :; do :; done"); err == nil {
		t.Fatal("hung command succeeded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("hung command took %s", elapsed)
	}
}

func TestWinRoot(t *testing.T) {
	drvfs := func(point string) []pathview.Mount {
		return []pathview.Mount{{Point: point, Kind: pathview.KindDrvFS, Drive: "C"}}
	}
	for _, tc := range []struct {
		name   string
		mounts []pathview.Mount
		want   string
	}{
		{"custom mount", drvfs("/custom/c"), "/custom/c"},
		{"no drvfs mount", nil, "/mnt/c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := winRootFrom(pathview.NewResolver(tc.mounts)); got != tc.want {
				t.Errorf("winRoot = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunCommandPowerShellOffPathUsesWindowsRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("WSL-only fallback")
	}
	t.Setenv("PATH", t.TempDir()) // no powershell.exe on PATH
	_, err := runCommand(5*time.Second, "powershell.exe", "-NoProfile", "-Command", "exit 0")
	// Unfixed, the bare name fails LookPath with ErrNotFound; fixed, it execs the absolute path (found or ENOENT on it).
	if errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("powershell.exe was not redirected to the Windows root: %v", err)
	}
	var pe *fs.PathError
	if err != nil && errors.As(err, &pe) && !strings.HasSuffix(pe.Path,
		"/Windows/System32/WindowsPowerShell/v1.0/powershell.exe") {
		t.Errorf("exec'd %q", pe.Path)
	}
}

// NewEnv per platform: macOS keeps app data under Library/Application Support, Linux under .config, and only a
// Linux host that the WSL probe recognizes is WSL.
func TestNewEnvPerPlatform(t *testing.T) {
	for _, tc := range []struct {
		goos     string
		wslProbe bool
		appSup   []string
		wantWSL  bool
	}{
		{"darwin", true, []string{"Library", "Application Support"}, false},
		{"linux", false, []string{".config"}, false},
		{"linux", true, []string{".config"}, true},
		{"windows", true, []string{".config"}, false},
	} {
		t.Run(fmt.Sprintf("%s/wsl=%v", tc.goos, tc.wslProbe), func(t *testing.T) {
			origGOOS, origWSL := envGOOS, onWSL
			envGOOS, onWSL = tc.goos, func() bool { return tc.wslProbe }
			t.Cleanup(func() { envGOOS, onWSL = origGOOS, origWSL })
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)

			e := NewEnv()
			want := filepath.Join(append([]string{home}, tc.appSup...)...)
			if e.GOOS != tc.goos || e.WSL != tc.wantWSL || e.AppSupport != want {
				t.Fatalf("NewEnv = GOOS %q WSL %v AppSupport %q; want %q, %v, %q",
					e.GOOS, e.WSL, e.AppSupport, tc.goos, tc.wantWSL, want)
			}
		})
	}
}
