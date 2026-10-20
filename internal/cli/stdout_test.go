package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func closedStdout(t *testing.T) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "closed-stdout"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	t.Cleanup(func() { os.Stdout = old })
}

func TestMenuSVGReportsStdoutFailure(t *testing.T) {
	closedStdout(t)
	if got := cmdMenuSVG(nil); got == 0 {
		t.Fatal("help-svg exit = 0 on closed stdout, want nonzero")
	}
}

func TestWriteSettingsOutputReportsStdoutFailure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		out    string
		dryRun bool
	}{
		{name: "stdout"},
		{name: "dry run", out: "not-written.json", dryRun: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			closedStdout(t)
			if _, err := writeSettingsOutput(tt.out, false, tt.dryRun, []byte("{}\n")); err == nil {
				t.Fatal("writeSettingsOutput on closed stdout returned nil error")
			}
		})
	}
	t.Run("check does not write", func(t *testing.T) {
		closedStdout(t)
		if drift, err := writeSettingsOutput("", true, false, []byte("{}\n")); drift || err != nil {
			t.Fatalf("check-only output = %v, %v, want false, nil", drift, err)
		}
	})
}

func TestDispatchStartReportsStdoutFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	hermeticDispatch(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nprintf 'remote output\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	closedStdout(t)
	if got := dispatchStart(context.Background(), "stdout-test.invalid", "", "claude", "cc-stdout-test", "hello", false); got == 0 {
		t.Fatal("dispatch start exit = 0 on closed stdout, want nonzero")
	}
}
