package browser

import (
	"runtime"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/pathview"
)

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
