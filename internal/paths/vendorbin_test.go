package paths_test

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
)

func TestVendorBinCases(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "unknown name",
			run: func(t *testing.T) {
				_, err := paths.VendorBin("unknown")
				if err == nil || !strings.Contains(err.Error(), `unknown vendor "unknown"`) {
					t.Fatalf("VendorBin(unknown) error = %v, want unknown vendor message", err)
				}
			},
		},
		{
			name: "missing gemini names path and install tool",
			run: func(t *testing.T) {
				_, err := paths.VendorBin("gemini")
				if err == nil {
					t.Fatal("VendorBin(gemini) succeeded for missing binary")
				}
				for _, fragment := range []string{
					filepath.Join(paths.BinDir(), "gemini"),
					"--tools=agy",
				} {
					if !strings.Contains(err.Error(), fragment) {
						t.Errorf("VendorBin(gemini) error %q does not contain %q", err, fragment)
					}
				}
			},
		},
		{
			name: "dangling symlink is broken link",
			run: func(t *testing.T) {
				binDir := paths.BinDir()
				if err := os.MkdirAll(binDir, 0o755); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(binDir, "codex")
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), link); err != nil {
					t.Fatal(err)
				}
				_, err := paths.VendorBin("codex")
				for _, fragment := range []string{"broken link", link} {
					if err == nil || !strings.Contains(err.Error(), fragment) {
						t.Errorf("VendorBin(codex) error = %v, want substring %q", err, fragment)
					}
				}
			},
		},
		{
			name: "symlink to executable is retired shim",
			run: func(t *testing.T) {
				binDir := paths.BinDir()
				if err := os.MkdirAll(binDir, 0o755); err != nil {
					t.Fatal(err)
				}
				self, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(binDir, "codex")
				if err := os.Symlink(self, link); err != nil {
					t.Fatal(err)
				}
				_, err = paths.VendorBin("codex")
				for _, fragment := range []string{"retired claude-code shim", link} {
					if err == nil || !strings.Contains(err.Error(), fragment) {
						t.Errorf("VendorBin(codex) error = %v, want substring %q", err, fragment)
					}
				}
			},
		},
		{
			name: "symlink to claude-code file is retired shim",
			run: func(t *testing.T) {
				binDir := paths.BinDir()
				if err := os.MkdirAll(binDir, 0o755); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "claude-code")
				if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(binDir, "codex")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				_, err := paths.VendorBin("codex")
				for _, fragment := range []string{"retired claude-code shim", link} {
					if err == nil || !strings.Contains(err.Error(), fragment) {
						t.Errorf("VendorBin(codex) error = %v, want substring %q", err, fragment)
					}
				}
			},
		},
		{
			name: "0644 file is not executable",
			run: func(t *testing.T) {
				link := filepath.Join(paths.BinDir(), "codex")
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(link, []byte("not executable"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(link, 0o644); err != nil {
					t.Fatal(err)
				}
				_, err := paths.VendorBin("codex")
				for _, fragment := range []string{"not executable", link} {
					if err == nil || !strings.Contains(err.Error(), fragment) {
						t.Errorf("VendorBin(codex) error = %v, want substring %q", err, fragment)
					}
				}
			},
		},
		{
			name: "directory is not executable",
			run: func(t *testing.T) {
				link := filepath.Join(paths.BinDir(), "codex")
				if err := os.MkdirAll(link, 0o755); err != nil {
					t.Fatal(err)
				}
				_, err := paths.VendorBin("codex")
				for _, fragment := range []string{"not executable", link} {
					if err == nil || !strings.Contains(err.Error(), fragment) {
						t.Errorf("VendorBin(codex) error = %v, want substring %q", err, fragment)
					}
				}
			},
		},
		{
			name: "regular file returns bin path",
			run: func(t *testing.T) {
				got := pathstest.FakeVendor(t, "codex", "exit 0")
				want := filepath.Join(paths.BinDir(), "codex")
				if got != want {
					t.Fatalf("FakeVendor path = %q, want %q", got, want)
				}
				if resolved, err := paths.VendorBin("codex"); err != nil || resolved != want {
					t.Fatalf("VendorBin(codex) = %q, %v; want %q, nil", resolved, err, want)
				}
			},
		},
		{
			name: "agy and gemini resolve to gemini bin path",
			run: func(t *testing.T) {
				pathstest.FakeVendor(t, "agy", "exit 0")
				want := filepath.Join(paths.BinDir(), "gemini")
				for _, name := range []string{"agy", "gemini"} {
					got, err := paths.VendorBin(name)
					if err != nil || got != want {
						t.Errorf("VendorBin(%q) = %q, %v; want %q, nil", name, got, err, want)
					}
				}
			},
		},
		{
			name: "symlink returns its own path",
			run: func(t *testing.T) {
				binDir := paths.BinDir()
				if err := os.MkdirAll(binDir, 0o755); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "codex")
				if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, 0o755); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(binDir, "codex")
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
				got, err := paths.VendorBin("codex")
				if err != nil || got != link {
					t.Fatalf("VendorBin(codex) = %q, %v; want link path %q", got, err, link)
				}
				if got == target {
					t.Fatalf("VendorBin(codex) returned target %q instead of link %q", got, link)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			tt.run(t)
		})
	}
}

func TestVendorName(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())

	if got, ok := paths.VendorName("agy"); !ok || got != "gemini" {
		t.Fatalf("VendorName(agy) = %q, %t; want gemini, true", got, ok)
	}
}

func TestFakeVendorRefusesRealConfig(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", u.HomeDir)

	tb := &fatalTB{TB: t}
	returned := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pathstest.FakeVendor(tb, "codex", "exit 0")
		returned <- struct{}{}
	}()
	<-done

	if !tb.called {
		t.Fatal("FakeVendor did not call Fatal")
	}
	for _, fragment := range []string{"refusing to write", "real ~/.claude"} {
		if !strings.Contains(tb.message, fragment) {
			t.Errorf("Fatal message %q does not contain %q", tb.message, fragment)
		}
	}
	select {
	case <-returned:
		t.Fatal("FakeVendor returned instead of stopping before writing")
	default:
	}
}

type fatalTB struct {
	testing.TB
	called  bool
	message string
}

func (tb *fatalTB) Fatal(args ...any) {
	tb.called = true
	tb.message = fmt.Sprint(args...)
	runtime.Goexit()
}

func (tb *fatalTB) Fatalf(format string, args ...any) {
	tb.called = true
	tb.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}
