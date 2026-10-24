package browser

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
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
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	saved := winRootTimeout
	winRootTimeout = 100 * time.Millisecond
	t.Cleanup(func() { winRootTimeout = saved })
	path := filepath.Join(dir, "wslpath")
	for _, tc := range []struct {
		name, script, want string
	}{
		{"custom mount", "printf ' /custom/c///\\n'", "/custom/c"},
		{"empty output", "printf ' / \\n'", "/mnt/c"},
		{"failed interop", "exit 1", "/mnt/c"},
		{"hung interop", "while :; do :; done", "/mnt/c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if got := winRoot(); got != tc.want {
				t.Errorf("winRoot = %q, want %q", got, tc.want)
			}
			if elapsed := time.Since(start); elapsed > 4*time.Second {
				t.Errorf("winRoot took %s", elapsed)
			}
		})
	}
}
